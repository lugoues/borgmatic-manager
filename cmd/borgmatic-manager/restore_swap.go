package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lugoues/borgmatic-manager/internal/config"
)

// stagingPrefix begins the name of the sibling directory a mirror restore
// extracts into. Sibling, not a subdirectory of the target: it has to be on the
// same filesystem for the swap to be a rename, and inside the target it would
// be part of what the restore is meant to replace.
//
// A prefix rather than a fixed suffix, completed with random characters by
// MkdirTemp. Clearing leftovers is the first thing a restore does, and against
// a fixed name that is an unconditional recursive delete of whatever happens to
// hold it. Nothing marks such a directory as this tool's, and for a
// symlink-backed volume it can sit in a directory some application owns. A name
// nobody else can have produced makes the question moot.
const stagingPrefix = ".borgmatic-manager-restoring-"

// oldPrefix begins the name of the displaced original on the fallback swap
// path. It only exists between two renames, and only where RENAME_EXCHANGE is
// unavailable.
//
// A prefix completed by MkdirTemp, for the same reason as stagingPrefix: the
// fallback used to clear a fixed name before renaming onto it, which
// recursively deleted whatever happened to be there. A symlink-backed _data
// resolves into a directory some application owns, and that made an unrelated
// directory collateral of a restore that had not yet touched the volume.
const oldPrefix = ".borgmatic-manager-replaced-"

// errStranded marks the one failure that must not clean up after itself: the
// swap left the old data under one name and the new data under another, with
// the volume's own path empty. Discarding either would take away a choice the
// operator now has to make by hand.
var errStranded = errors.New("restore stranded mid-swap")

// restoreWithSwap extracts into a staging sibling and only then replaces the
// live data, so the original survives every way the extract can fail: a
// mid-flight crash, a corrupt archive that lists cleanly but extracts
// partially, an archive pruned out from under the restore, or power loss.
//
// This is what makes the pre-flight archive check advisory rather than
// load-bearing. Emptying the target first and extracting into it means any
// failure after the wipe leaves an operator with no data and no restore, and
// no check performed beforehand can rule that out.
//
// extract receives the destination to write into and must return a non-nil
// error if it did not complete.
// allowEmpty says the archive holds this directory but no children, so an empty
// extract is the correct result rather than a sign that nothing matched.
// stillSafe is checked again immediately before the swap. The extract can take
// a long time, and a container started meanwhile would have mounted the very
// inode the swap is about to move out from under it.
func restoreWithSwap(targetData string, logger *slog.Logger, allowEmpty bool, extract func(destination string) error, stillSafe func() error) (err error) {
	// A previous run killed between extract and swap leaves these behind. They
	// are never live data (the swap renames rather than copies), so they are
	// safe to discard, and reusing one would mix two restores together. Only
	// this tool can have created a path with this prefix, and the per-volume
	// restore lock means no other run holds one right now.
	if rmErr := clearStagingLeftovers(targetData, logger); rmErr != nil {
		return rmErr
	}

	staging := ""

	// Registered before the directory exists, so a createStagingLike that fails
	// partway through its owner/mode/label work does not leave a half-built
	// directory behind. RemoveAll on a path that was never created is a no-op.
	//
	// Any failure before the swap discards the staging directory and leaves the
	// live data exactly as it was. The exceptions are a stranded swap and an
	// aborted recheck, where staging holds the only copy of the restored data.
	committed, keepStaging := false, false
	defer func() {
		if committed || keepStaging || staging == "" || errors.Is(err, errStranded) {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			logger.Warn("could not remove the staging directory; the volume itself is untouched",
				"staging", staging, "error", rmErr)
		}
	}()

	staging, createErr := createStagingLike(targetData, logger)
	if createErr != nil {
		return createErr
	}
	logger.Info("staging the restore", "path", staging)

	if extractErr := extract(staging); extractErr != nil {
		return fmt.Errorf("extracting into %s (the volume is untouched): %w", staging, extractErr)
	}

	entries, readErr := os.ReadDir(staging)
	if readErr != nil {
		return fmt.Errorf("reading back the extracted data (the volume is untouched): %w", readErr)
	}
	if len(entries) == 0 && !allowEmpty {
		return fmt.Errorf("borgmatic reported success but extracted nothing into %s, so the volume is untouched", staging)
	}

	// rename gives the namespace change atomically but says nothing about the
	// contents being on disk. Persist them before the original, which is the
	// only other durable copy, is deleted.
	if syncErr := syncStagedData(filepath.Dir(targetData)); syncErr != nil {
		return fmt.Errorf("flushing the restored data to disk (the volume is untouched): %w", syncErr)
	}

	// After the extract, before the swap: borg has to be able to write into
	// staging, so this is the last moment it can be made read-only and the
	// first at which doing so is harmless.
	if roErr := matchSubvolumeReadOnly(targetData, staging, logger); roErr != nil {
		return roErr
	}

	// Last check before the only destructive step.
	if stillSafe != nil {
		if safeErr := stillSafe(); safeErr != nil {
			// The extract is complete and may have taken hours. Keep it, but
			// move it off the staging path first: the retry the operator is
			// about to run clears staging as its first act, which would delete
			// the very tree this error points them at.
			keepStaging = true
			kept := retainOrWarn(staging, targetData, logger)
			return fmt.Errorf("%w (the volume is untouched; the completed restore is kept at %s)", safeErr, kept)
		}
	}

	displaced, swapErr := swapIntoPlaceFn(staging, targetData, logger)
	if swapErr != nil {
		return swapErr
	}
	committed = true
	// The mark comes off only now. Removing it before the swap left a window in
	// which a kill produced a complete, volume-sized staging tree with no proof
	// of ownership on it, which the next restore would then refuse to delete and
	// leave on disk indefinitely. Staging stays provably ours right up to the
	// moment it stops being staging.
	//
	// Best effort on the way out: this is the live volume now, its name no
	// longer matches the scratch pattern, so nothing looks at the mark again. A
	// read-only subvolume cannot have it removed at all, and failing a completed
	// restore over a stray attribute would be absurd.
	if unmarkErr := removeXattr(targetData, scratchMarkerAttr); unmarkErr != nil {
		logger.Warn("the restored volume kept an internal scratch mark, which is harmless but untidy; "+
			"remove it with: setfattr -x "+scratchMarkerAttr,
			"path", targetData, "error", unmarkErr)
	}
	// Persist the swap itself before removing the copy it replaced, or a power
	// loss could replay the deletion without the rename and lose both.
	if syncErr := syncDir(filepath.Dir(targetData)); syncErr != nil {
		// Move it off the staging path before reporting it. The next restore
		// clears staging as its first act, which would silently delete the very
		// copy this branch is telling the operator to keep.
		kept := retainOrWarn(displaced, targetData, logger)
		logger.Warn("the restore is in place but the swap could not be flushed to disk, so the copy it replaced is being kept; "+
			"delete it once the volume looks right",
			"path", filepath.Dir(targetData), "kept", kept, "error", syncErr)
		return nil
	}
	// The restore is done and durable. Failing to delete the copy it replaced is
	// wasted disk, not a failed restore, so it must not become a nonzero exit
	// for an operation that succeeded.
	if rmErr := removeScratchDir(displaced); rmErr != nil {
		// On the exchange path this *is* the staging path, and the next restore
		// clears staging as its very first act and refuses to start if it
		// cannot. Left here, a restore that succeeded today becomes the reason
		// the next one fails, so move it aside rather than leave it in the way.
		kept := retainOrWarn(displaced, targetData, logger)
		logger.Warn("restore succeeded, but the data it replaced could not be removed; delete it to reclaim the space",
			"path", kept, "error", rmErr)
	}
	logger.Info("restore complete", "path", targetData, "entries", len(entries))
	return nil
}

// moveAsideFn is the seam for the fallback's first rename, so a test can look at
// the displaced directory at the instant it exists rather than after the fact.
// The whole point of marking before the rename is what is true in that instant.
var moveAsideFn = func(targetData, displaced string) error {
	return unix.Renameat(unix.AT_FDCWD, targetData, unix.AT_FDCWD, displaced)
}

// swapIntoPlaceFn is the seam for the one irreversible step, so a test can
// inspect what is about to be promoted at the moment it is promoted.
var swapIntoPlaceFn = swapIntoPlace

// swapIntoPlace makes staging the live data directory and returns the path the
// displaced original ended up at, for the caller to dispose of.
//
// RENAME_EXCHANGE trades the two directories in one atomic step, so there is no
// instant at which the volume has no data directory, even across power loss.
// Filesystems without it fall back to a pair of renames, which leaves a window
// of exactly two syscalls; recoverInterruptedSwap repairs that window if the
// process dies inside it.
func swapIntoPlace(staging, targetData string, logger *slog.Logger) (displaced string, err error) {
	// The exchange sends the live inode to the staging name, and that inode has
	// never been this tool's, so it arrives there carrying no proof. A kill
	// before the cleanup below then leaves a volume-sized directory under a
	// staging name that no later run will delete, because it cannot tell it from
	// something an application put there. Claiming it first costs one syscall
	// and travels with the inode through the exchange.
	claimed := markAsOurs(targetData)
	switch exErr := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, targetData, unix.RENAME_EXCHANGE); {
	case exErr == nil:
		// The directories traded places: the old data is now under the staging
		// name, which is what the caller removes.
		return staging, nil
	case claimed:
		// It did not happen, so the claim on the live volume means nothing and
		// must not be left behind. Cleared on every path out of the switch that
		// is not the exchange itself.
		_ = removeXattr(targetData, scratchMarkerAttr)
		fallthrough
	case errors.Is(exErr, unix.ENOSYS), errors.Is(exErr, unix.EINVAL),
		errors.Is(exErr, unix.EOPNOTSUPP), errors.Is(exErr, unix.ENOTSUP):
		// No atomic exchange on this kernel or filesystem; use the rename pair.
	default:
		return "", fmt.Errorf("swapping the restored data into place (the volume is untouched): %w", exErr)
	}

	return swapByRenamePair(staging, targetData, logger)
}

// swapByRenamePair is the swap for filesystems without RENAME_EXCHANGE. It is
// separate so the rollback below can be exercised directly: on a kernel that
// does support the atomic exchange, nothing else can reach this code.
func swapByRenamePair(staging, targetData string, logger *slog.Logger) (displaced string, err error) {
	// Created rather than merely named, so the name cannot already belong to
	// something else. unix.Renameat rather than os.Rename below because Go's
	// wrapper refuses any existing directory while the syscall replaces an empty
	// one, which is exactly what this placeholder is.
	displaced, err = os.MkdirTemp(filepath.Dir(targetData), scratchPattern(targetData, oldPrefix))
	if err != nil {
		return "", fmt.Errorf("reserving a name to move the current data aside (the volume is untouched): %w", err)
	}
	markAsOurs(displaced)
	// Marked before it moves, not after it lands. Extended attributes belong to
	// the inode and travel with it through a rename, so claiming the volume's
	// own directory here means the displaced copy carries proof from the instant
	// it exists. Marking the placeholder instead was useless (the rename
	// discards that inode) and marking afterwards left a window: a kill between
	// the rename and the mark produced a missing volume beside an unverifiable
	// directory, which recovery then refused and left for the operator to sort
	// out by hand.
	if !markAsOurs(targetData) {
		// Said before the window opens, not after something has gone wrong in
		// it. Without a claim on the inode an interruption in the next two
		// syscalls leaves a volume that recovery cannot put back automatically,
		// because it will have no way to tell the displaced copy from an
		// unrelated directory. The restore is still worth doing: both copies
		// survive and are named here, so the recovery is manual rather than
		// impossible.
		logger.Warn("this filesystem cannot record which directory belongs to this restore, and it also has no "+
			"atomic directory exchange, so if the restore is interrupted in the next moment the volume will have "+
			"to be put back by hand from the path below",
			"volume", targetData, "displaced_to", displaced)
	}
	if renErr := moveAsideFn(targetData, displaced); renErr != nil {
		// It never moved, so the claim on it is meaningless and would otherwise
		// stay on the live volume.
		_ = removeXattr(targetData, scratchMarkerAttr)
		if rmErr := os.Remove(displaced); rmErr != nil {
			return "", fmt.Errorf("moving the current data aside (the volume is untouched): %w",
				errors.Join(renErr, rmErr))
		}
		return "", fmt.Errorf("moving the current data aside (the volume is untouched): %w", renErr)
	}
	if renErr := os.Rename(staging, targetData); renErr != nil {
		// Put it back: a volume with no data directory is worse than a failed
		// restore, and this is the only moment that state can exist.
		if back := os.Rename(displaced, targetData); back != nil {
			return "", fmt.Errorf("%w: the volume has no data directory. The data as it was is at %s, "+
				"and the restored copy is at %s; move whichever you want to %s by hand: %w",
				errStranded, displaced, staging, targetData, errors.Join(renErr, back))
		}
		// It is the volume again, and the claim travelled back with the inode.
		// Left on, the next run would treat the live volume as scratch of its
		// own if it ever appeared under one of these names.
		_ = removeXattr(targetData, scratchMarkerAttr)
		return "", fmt.Errorf("moving the restored data into place (the volume is unchanged): %w", renErr)
	}
	return displaced, nil
}

// recoverInterruptedSwap repairs a volume whose data directory is missing but
// whose displaced original is still on disk: the state left if the process dies
// between the fallback path's two renames. Without this the next restore stops
// at "data directory is not present" and the operator has to know to go looking
// for a suffixed directory.
// resolveVolumeData resolves a volume's _data to the directory it actually
// names, so staging lands beside the real directory and the swap replaces that
// rather than the link pointing at it.
//
// A missing final component is not an error here. When _data is a symlink into
// a backing directory, an interrupted rename-pair swap leaves that directory
// under its suffixed name and the link dangling, which is precisely the state
// recovery exists to repair. EvalSymlinks fails outright on it, so the last
// component is resolved by hand and only the path above it is required to
// exist.
func resolveVolumeData(targetData string) (string, error) {
	// Bounded so a symlink loop cannot spin here. The kernel's own limit is 40.
	const maxLinks = 40

	current := targetData
	for i := 0; i < maxLinks; i++ {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return resolved, nil
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// Not a symlink, so there is nothing here to resolve. Whatever is
			// wrong with the path is reported by the caller's own existence
			// check, which can say more about it than this function can.
			return current, nil
		}
		link, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("reading the symlink at %s: %w", current, err)
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(current), link)
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(link))
		if err != nil {
			return "", fmt.Errorf("%s points into %s, which could not be resolved: %w", current, filepath.Dir(link), err)
		}
		// Loop rather than return: the thing this names may itself be another
		// symlink, and only the final component is allowed to be missing. A
		// chain that stops at an intermediate link leaves recovery looking for
		// the displaced copy beside the wrong directory.
		current = filepath.Join(parent, filepath.Base(link))
	}
	return "", fmt.Errorf("resolving %s followed more than %d symlinks; it is probably a loop", targetData, maxLinks)
}

func recoverInterruptedSwap(targetData string, logger *slog.Logger) error {
	matches, enumerable, err := siblingsWithPrefix(targetData, oldPrefix)
	if err != nil {
		return fmt.Errorf("looking for data displaced by an interrupted restore beside %s: %w", targetData, err)
	}
	if !enumerable {
		// Recovery cannot help here, but a merge or in-place restore never
		// needed it, so this is a warning rather than the end of the run.
		logger.Warn("the volume's parent directory cannot be listed, so data displaced by an interrupted "+
			"restore cannot be found; if the volume is missing, look beside it by hand",
			"parent", filepath.Dir(targetData))
		return nil
	}
	if len(matches) == 0 {
		return nil
	}

	targetPresent := true
	if _, statErr := os.Lstat(targetData); statErr != nil && os.IsNotExist(statErr) {
		targetPresent = false
	}

	// Whether an empty match is rubbish or is the volume depends entirely on
	// this. While the target is there, an empty one can only be a name the
	// fallback reserved and never renamed onto, left by a process that died in
	// the two syscalls between; nothing was displaced, because the volume is
	// still in place. Once the target is gone, an empty one may be the volume
	// itself, because a volume is allowed to be empty. Reaping those
	// unconditionally would throw away the very directory recovery exists to put
	// back.
	if targetPresent {
		for _, m := range matches {
			// Ownership first. Asking whether an unrelated directory is empty
			// needs permission to read it, and an application-owned one this
			// user cannot read would otherwise abort every restore before the
			// check that would have dismissed it ever ran.
			ours, known, ownErr := provablyOurs(m)
			if ownErr != nil {
				return ownErr
			}
			if !ours || !known {
				logger.Warn("a directory beside this volume is named like one this tool creates but carries no "+
					"mark showing it did, so it is being left alone", "path", m)
				continue
			}
			empty, emptyErr := isEmptyDir(m)
			if emptyErr != nil {
				return emptyErr
			}
			if !empty {
				logger.Warn("a directory displaced by an earlier restore is beside this volume and holds data; "+
					"the volume itself is fine, so remove it once you have looked at it",
					"path", m, "volume", targetData)
				continue
			}
			// Left in place these accumulate, and a later genuine interruption
			// then produces two matches and a refusal to choose, turning a
			// recoverable volume into a manual job.
			if rmErr := os.Remove(m); rmErr != nil {
				return fmt.Errorf("removing an abandoned reservation %s: %w", m, rmErr)
			}
			logger.Warn("removed a name reserved by a restore that did not finish", "path", m)
		}
		return nil
	}

	// The target is gone, so one of these is the volume. A non-empty one is
	// certainly it; an empty one alongside can only be an earlier abandoned
	// reservation, since a swap displaces exactly one directory.
	// Ownership first, and nothing that fails it is a candidate at all. A name
	// is not evidence: the volume being missing for some unrelated reason,
	// beside a directory that merely matches the public shape, would otherwise
	// have this rename someone else's data into the volume's path and present it
	// as the volume.
	var ours []string
	for _, m := range matches {
		mine, known, ownErr := provablyOurs(m)
		if ownErr != nil {
			return ownErr
		}
		switch {
		case !known:
			logger.Warn("a directory beside this volume may hold data displaced by an interrupted restore, but "+
				"it cannot be read, so it cannot be verified as this tool's and is being left alone; if this "+
				"volume is missing, check that directory and rename it into place yourself",
				"path", m, "volume", targetData)
			continue
		case !mine:
			logger.Warn("a directory beside this volume matches the name an interrupted restore leaves behind "+
				"but carries no mark showing this tool displaced it, so it is being left alone",
				"path", m, "volume", targetData)
			continue
		}
		ours = append(ours, m)
	}
	if len(ours) == 0 {
		return nil // nothing here is this tool's to put back
	}

	// Emptiness only matters when there is a choice to make. Reading a
	// directory needs permission that putting it back does not, so a volume the
	// owner can search and write but not list (mode 0300) would otherwise be
	// unrecoverable for a question nobody asked.
	displaced := ours[0]
	if len(ours) > 1 {
		var withData []string
		for _, m := range ours {
			empty, emptyErr := isEmptyDir(m)
			if emptyErr != nil {
				return emptyErr
			}
			if !empty {
				withData = append(withData, m)
			}
		}
		if len(withData) > 1 {
			// Never expected: the window is two syscalls wide and one restore of
			// a volume runs at a time. Choosing between two copies of a volume's
			// data is not this program's to do.
			return fmt.Errorf("%s is missing and more than one displaced copy beside it holds data (%s); "+
				"a restore was interrupted more than once, so move the one you want to %s by hand",
				targetData, strings.Join(withData, ", "), targetData)
		}
		// With no non-empty candidate every one of ours is empty, and the volume
		// was empty too: any of them restores the same directory.
		if len(withData) == 1 {
			displaced = withData[0]
		}
	}

	if err := os.Rename(displaced, targetData); err != nil {
		return fmt.Errorf("a previous restore was interrupted mid-swap and %s could not be moved back to %s: %w",
			displaced, targetData, err)
	}
	// Durable before it is reported. This restore can still fail for a dozen
	// reasons before it reaches a swap of its own, and an unflushed recovery can
	// be rolled back by a power loss after the operator has been told the volume
	// was put back, leaving _data missing again with nothing left saying why.
	if syncErr := syncDirFn(filepath.Dir(targetData)); syncErr != nil {
		return fmt.Errorf("a previous restore was interrupted mid-swap and %s was moved back to %s, "+
			"but that could not be flushed to disk, so it may not survive a power loss: %w",
			displaced, targetData, syncErr)
	}
	// It is the volume again, so the scratch mark no longer applies. Harmless if
	// it cannot be removed: nothing consults it at this path.
	if unmarkErr := removeXattr(targetData, scratchMarkerAttr); unmarkErr != nil {
		logger.Warn("the recovered volume kept an internal scratch mark, which is harmless but untidy",
			"path", targetData, "error", unmarkErr)
	}
	logger.Warn("a previous restore was interrupted mid-swap; the volume's data has been put back",
		"path", targetData, "recovered_from", displaced)

	// Anything still beside it was an abandoned reservation, now provably so.
	// Only ours: the unverified ones were excluded above and are not this
	// function's to delete.
	for _, m := range ours {
		if m == displaced {
			continue
		}
		if rmErr := os.Remove(m); rmErr != nil {
			logger.Warn("could not remove a name reserved by a restore that did not finish",
				"path", m, "error", rmErr)
		}
	}
	return nil
}

// isEmptyDir reports whether path is a directory with no entries. A non
// directory is never empty in the sense this asks about: it is something this
// program did not create and must not remove.
func isEmptyDir(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// createStagingLike makes the staging directory carry the same identity as the
// directory it will replace: owner, mode including the special bits, and
// SELinux context. A fresh directory would otherwise get the creating process's
// defaults, and on an SELinux host a volume relabelled that way stops being
// readable by the container that owns it.
func createStagingLike(model string, logger *slog.Logger) (staging string, err error) {
	info, err := os.Stat(model)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", model, err)
	}
	// Read the label before creating anything: an unreadable one is a reason to
	// stop, not something to discover after the directory exists.
	label, labelled, err := readSELinuxContext(model)
	if err != nil {
		return "", err
	}

	staging, err = createStagingDir(model, logger)
	if err != nil {
		return "", fmt.Errorf("creating a staging directory beside %s: %w", model, err)
	}
	// From here the directory exists, so every failure has to take it with it.
	// The caller cannot: it only learns the name from a successful return, and a
	// half-built directory left on disk is one the next restore has to reason
	// about.
	//
	// Held in its own variable because the failure paths below return an empty
	// name, which would leave this cleanup with nothing to remove.
	created := staging
	defer func() {
		if err != nil {
			if rmErr := removeScratchDir(created); rmErr != nil {
				logger.Warn("could not remove a staging directory that could not be set up; "+
					"the volume itself is untouched", "path", created, "error", rmErr)
			}
		}
	}()
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		if err := os.Chown(staging, int(st.Uid), int(st.Gid)); err != nil {
			return "", fmt.Errorf("setting the owner on %s: %w", staging, err)
		}
	}
	// After Chown, which clears setuid and setgid.
	mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if err := os.Chmod(staging, mode); err != nil {
		return "", fmt.Errorf("setting the mode on %s: %w", staging, err)
	}
	if labelled {
		if err := applySELinuxContext(staging, label); err != nil {
			return "", err
		}
	} else if err := clearInheritedSELinuxContext(staging, logger); err != nil {
		return "", err
	}
	if err := copyAccessControlLists(model, staging); err != nil {
		return "", err
	}
	if err := copyRemainingXattrs(model, staging, logger); err != nil {
		return "", err
	}
	if err := copyInodeFlags(model, staging, logger); err != nil {
		return "", err
	}
	warnAboutProjectQuota(model, staging, logger)
	return staging, nil
}

// inheritableInodeFlags are the FS_IOC_GETFLAGS bits a directory passes on to
// what is created inside it, so they have to be on staging *before* borg writes
// anything: setting nodatacow afterwards does not convert files that already
// exist. btrfs +C on a database volume is the case that matters, where losing
// it silently restores copy-on-write behaviour the operator deliberately turned
// off.
//
// Defined here because golang.org/x/sys/unix exports the ioctls but not the
// flag bits. Values are from linux/fs.h and are stable ABI.
const (
	fsNoCoWFlag      = 0x00800000 // FS_NOCOW_FL
	fsCompressFlag   = 0x00000004 // FS_COMPR_FL
	fsNoAtimeFlag    = 0x00000080 // FS_NOATIME_FL
	fsNoDumpFlag     = 0x00000040 // FS_NODUMP_FL
	fsSyncFlag       = 0x00000008 // FS_SYNC_FL
	fsDirSyncFlag    = 0x00010000 // FS_DIRSYNC_FL
	fsNoCompressFlag = 0x00000400 // FS_NOCOMP_FL
	fsDaxFlag        = 0x02000000 // FS_DAX_FL
	fsCasefoldFlag   = 0x40000000 // FS_CASEFOLD_FL
	inheritableMask  = fsNoCoWFlag | fsCompressFlag | fsNoCompressFlag | fsNoAtimeFlag |
		fsNoDumpFlag | fsSyncFlag | fsDirSyncFlag | fsDaxFlag | fsCasefoldFlag
)

// copyInodeFlags carries the model's inheritable inode flags onto staging.
//
// These are ioctl state rather than extended attributes, so none of the xattr
// copying reaches them. Only the inheritable ones are copied: flags like
// immutable and append-only describe the directory itself rather than what goes
// in it, and reproducing those on a directory about to be extracted into would
// break the extract.
func copyInodeFlags(model, staging string, logger *slog.Logger) error {
	flags, ok, err := readInodeFlags(model)
	if err != nil {
		return err
	}
	if !ok {
		return nil // this filesystem has no inode flags to carry
	}
	// The target's exact state across the mask, not merely its set bits. A fresh
	// directory inherits these from its parent, so one the target does *not*
	// have can arrive on staging by itself: a parent marked nodatacow gives it
	// to a staging sibling even where _data was deliberately left copy-on-write,
	// and only replacing the masked bits can take it back off. That also means
	// there is no shortcut for a target with none of them set, which is exactly
	// the case where an inherited flag would otherwise survive unnoticed.
	if setErr := setInodeFlagsExactly(staging, flags&inheritableMask); setErr != nil {
		// Not fatal. The data lands correctly either way, and refusing to
		// restore a volume over a storage-behaviour flag is the worse outcome;
		// but it changes how every restored file is stored, so it is said out
		// loud rather than dropped.
		logger.Warn("could not apply the volume's inode flags to the directory replacing it, so the restored "+
			"files will not inherit them (btrfs +C in particular cannot be applied after the files exist)",
			"path", staging, "flags", fmt.Sprintf("%#x", flags&inheritableMask), "error", setErr)
	}
	return nil
}

// readInodeFlags reports a path's inode flags, and whether the filesystem
// answers at all. A filesystem without the ioctl has no flags to lose.
var readInodeFlags = func(path string) (int, bool, error) {
	// #nosec G304 -- a directory this process is restoring
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, false, fmt.Errorf("opening %s to read its inode flags: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS) ||
			errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading the inode flags on %s: %w", path, err)
	}
	return flags, true, nil
}

// setInodeFlagsExactly makes the masked bits on path equal want, leaving every
// flag outside the mask as it is.
var setInodeFlagsExactly = func(path string, want int) error {
	// #nosec G304 -- a directory this process just created
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	current, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return err
	}
	desired := (current &^ inheritableMask) | (want & inheritableMask)
	if desired == current {
		return nil
	}
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, desired)
}

// warnAboutProjectQuota reports a project quota that the replacement directory
// will not inherit.
//
// A project id and its inherit flag live in the inode's fsxattr, reached by
// ioctl rather than as an extended attribute, so none of the copying above
// carries them: a new directory starts at project 0. The volume and everything
// later written under it then sit outside the quota they were meant to be in,
// and nothing else says so.
//
// A warning rather than a copy. Reproducing the id means issuing
// FS_IOC_FSGETXATTR and FS_IOC_FSSETXATTR by hand through unsafe pointers, and
// no filesystem supporting project quotas can be mounted in the environment
// this was written in, so that code could not have been tested. Untested
// pointer work in a restore path is a worse trade than an accurate warning.
func warnAboutProjectQuota(model, staging string, logger *slog.Logger) {
	want, wantKnown := projectQuotaID(model)
	got, gotKnown := projectQuotaID(staging)
	if !wantKnown || !gotKnown || want == got {
		return
	}
	// Both directions matter, and comparing against zero caught only one of
	// them. A parent with a project id and PROJINHERIT gives staging that id,
	// so a volume charged to nothing can be promoted into someone else's quota
	// as easily as one under a quota can escape it.
	logger.Warn("the directory replacing this volume has a different filesystem project quota id, which lives "+
		"in the inode rather than in an extended attribute and cannot be copied with the rest; the restored "+
		"volume will be charged to the project shown as replacement_project_id until it is set again (chattr -p)",
		"path", model, "project_id", want, "replacement_project_id", got)
}

// projectQuotaID reports the project quota id on path, and whether it could be
// established at all. Shelling out for the same reason the btrfs checks do, and
// an unreadable or unsupported answer is "unknown" rather than "zero".
var projectQuotaID = func(path string) (uint64, bool) {
	// #nosec G204 -- fixed argv over a path this process derived
	out, err := exec.Command("lsattr", "-pd", path).Output()
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, false
	}
	id, convErr := strconv.ParseUint(fields[0], 10, 64)
	if convErr != nil {
		return 0, false
	}
	return id, true
}

// canCreateSibling reports whether a staging directory can be created beside
// targetData.
//
// Answered by trying, not by inspecting permission bits: access(2) asks about
// the real uid rather than the effective one, and a filesystem can refuse for
// reasons no mode bit shows (read-only mount, an LSM, a full inode table). The
// staged swap needs to create an entry in the *parent*, where the previous
// in-place restore only ever wrote inside _data, so a volume whose parent is
// not writable becomes impossible to mirror unless this is noticed first.
func canCreateSibling(targetData string) (bool, error) {
	probe, err := os.MkdirTemp(filepath.Dir(targetData), scratchPattern(targetData, stagingPrefix))
	if err != nil {
		// ENAMETOOLONG belongs here rather than with the genuine failures: the
		// scratch name is the target's own name plus a prefix and a random
		// suffix, so a target with a long but perfectly valid basename can have
		// no room left for one. That is a reason to restore in place, not a
		// reason to refuse a volume the previous implementation could restore.
		if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EROFS) || errors.Is(err, unix.ENAMETOOLONG) {
			return false, nil
		}
		return false, fmt.Errorf("checking whether a staging directory can be created beside %s: %w", targetData, err)
	}
	if rmErr := os.Remove(probe); rmErr != nil {
		return false, fmt.Errorf("removing the staging probe %s: %w", probe, rmErr)
	}
	return true, nil
}

// siblingsWithPrefix lists the paths beside targetData whose names begin with
// targetData's own name plus prefix.
//
// Reading the directory rather than globbing, because targetData is not a
// pattern this program chose: a symlink-backed volume resolves to whatever the
// link names, and a literal *, ? or [ in that path would be read as glob
// syntax. An unmatched bracket fails every staged restore, and a wildcard can
// match a different volume's live staging directory and have it deleted.
func siblingsWithPrefix(targetData, prefix string) (siblings []string, enumerable bool, err error) {
	parent := filepath.Dir(targetData)
	entries, err := os.ReadDir(parent)
	if err != nil {
		// Write and search without read is a real mode (0300), and the previous
		// in-place restore needed no permission to enumerate the parent at all.
		// Not being able to look is not the same as there being nothing there,
		// and it must not stop a restore that does not depend on looking.
		if errors.Is(err, os.ErrPermission) {
			return nil, false, nil
		}
		return nil, false, err
	}
	want := filepath.Base(targetData) + prefix
	var out []string
	for _, e := range entries {
		if madeByMkdirTemp(e.Name(), want) {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	return out, true, nil
}

// scratchMarkerAttr is set on every scratch directory this tool creates beside a
// volume, so cleanup can delete on proof rather than on a filename.
//
// A name only ever showed that something *looks* like ours. The prefix is
// public and the digits after it are not a signature, so a directory an
// application happens to call <volume>.borgmatic-manager-restoring-1234 matched
// and was recursively deleted. An attribute this process wrote is a fact about
// who made the directory.
//
// It is removed just before the swap, so it never reaches the live volume, and
// it costs nothing in the emptiness check because an extended attribute is not
// a directory entry.
const scratchMarkerAttr = "user.borgmatic-manager-scratch"

// markAsOurs claims a scratch directory. Best effort: a filesystem without user
// extended attributes cannot hold the claim, and the consequence is only that
// cleanup will later leave the directory alone rather than delete it, which is
// the safe direction.
func markAsOurs(path string) bool {
	return setXattr(path, scratchMarkerAttr, []byte("1"), 0) == nil
}

// provablyOurs reports whether this tool created path, and whether the question
// could be answered at all.
//
// Reading a user extended attribute needs read permission on the directory,
// exactly as listing it does, so a volume its owner can search and write but
// not read (mode 0300) cannot be verified. That is a real limit of marking
// rather than something to work around: dropping verification for such a
// directory would put back the risk the mark exists to remove, which is renaming
// an unrelated directory into the volume's path and presenting it as the volume.
func provablyOurs(path string) (ours, known bool, err error) {
	_, present, err := readXattrFn(path, scratchMarkerAttr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return false, false, nil
		}
		return false, false, err
	}
	return present, true, nil
}

// scratchPattern builds the MkdirTemp pattern for a scratch directory beside
// targetData.
//
// The trailing star is the point. MkdirTemp replaces the *last* star in the
// pattern rather than appending to it, so a target basename that itself
// contains one has the random suffix substituted into the middle of the name:
// "star*name.borgmatic-manager-replaced-" becomes "star4201828873name...",
// which no longer begins with the prefix the recovery searches for. Supplying
// the final star keeps everything before it literal.
func scratchPattern(targetData, prefix string) string {
	return filepath.Base(targetData) + prefix + "*"
}

// madeByMkdirTemp reports whether name is one MkdirTemp could have produced for
// this prefix: the prefix followed by its random digits and nothing else.
//
// A bare prefix test is not enough. The prefix is public and predictable, so a
// directory an application happens to call <volume>.borgmatic-manager-restoring-cache
// matches it, and cleanup deletes whatever it matches. Requiring the exact shape
// MkdirTemp produces is what makes "this is ours" a fact rather than a guess.
func madeByMkdirTemp(name, prefix string) bool {
	suffix, found := strings.CutPrefix(name, prefix)
	if !found || suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// removeScratchDir deletes a directory this tool created beside a volume.
//
// A staging directory made for a btrfs subvolume target is itself a subvolume,
// and emptying one is not the same as removing it: on a mount that permits
// creation but not unprivileged subvolume removal, RemoveAll clears the contents
// and then fails on the root. Left there it is not merely litter, because the
// next restore tries the same removal and stops on it, so the volume becomes
// unrestorable until someone deletes it by hand.
func removeScratchDir(path string) error {
	rmErr := removeAllFn(path)
	if rmErr == nil {
		return nil
	}
	subvolume, subErr := isSubvolume(path)
	if subErr != nil || !subvolume {
		return rmErr
	}
	if delErr := deleteBtrfsSubvolume(path); delErr != nil {
		return errors.Join(rmErr, delErr)
	}
	return nil
}

// removeAllFn and isSubvolume are seams for a filesystem that empties a
// directory but refuses to remove its root, which is not one this environment
// can produce on demand.
var (
	removeAllFn = os.RemoveAll
	isSubvolume = isBtrfsSubvolumeRoot
)

// deleteBtrfsSubvolume removes a subvolume by the one mechanism that works
// where rmdir does not.
var deleteBtrfsSubvolume = func(path string) error {
	// #nosec G204 -- fixed argv over a path this process created
	out, err := exec.Command("btrfs", "subvolume", "delete", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs subvolume delete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clearStagingLeftovers removes staging directories a previous run died before
// finishing. Every one of them was created by MkdirTemp under this tool's
// prefix, so nothing else can own one.
func clearStagingLeftovers(targetData string, logger *slog.Logger) error {
	matches, enumerable, err := siblingsWithPrefix(targetData, stagingPrefix)
	if err != nil {
		return fmt.Errorf("looking for leftover staging directories beside %s: %w", targetData, err)
	}
	if !enumerable {
		logger.Warn("the volume's parent directory cannot be listed, so leftovers from an unfinished restore "+
			"cannot be found; the restore itself is unaffected", "parent", filepath.Dir(targetData))
		return nil
	}
	for _, stale := range matches {
		ours, known, ownErr := provablyOurs(stale)
		if ownErr != nil {
			return ownErr
		}
		if !ours || !known {
			logger.Warn("a directory beside this volume is named like a staging directory but carries no mark "+
				"showing this tool created it, so it is being left alone; remove it yourself if it is not wanted",
				"path", stale)
			continue
		}
		if rmErr := removeScratchDir(stale); rmErr != nil {
			return fmt.Errorf("clearing a leftover staging directory %s: %w", stale, rmErr)
		}
		logger.Warn("removed a staging directory left by a restore that did not finish", "path", stale)
	}
	return nil
}

// aclAttrs are the extended attributes POSIX ACLs live in. Dropping them on the
// replacement can revoke access the volume's users had, and the default ACL
// also governs what files created after the restore inherit, so they are copied
// with the same "an existing one must apply" rule as the SELinux label.
var aclAttrs = []string{"system.posix_acl_access", "system.posix_acl_default"}

func copyAccessControlLists(from, to string) error {
	for _, attr := range aclAttrs {
		value, present, err := readXattrFn(from, attr)
		if err != nil {
			return err
		}
		if !present {
			// Absent on the model means absent on the replacement, not "leave
			// whatever is there". Staging is created inside the parent, so a
			// default ACL on the parent lands on it by inheritance, and copying
			// only present values would promote a root granting access the
			// volume never granted and passing it on to everything created
			// under it afterwards.
			if rmErr := removeXattr(to, attr); rmErr != nil {
				return fmt.Errorf("the volume has no %s but one inherited onto %s could not be removed, "+
					"so a swap would change who can reach the restored data (nothing was changed): %w", attr, to, rmErr)
			}
			continue
		}
		if err := setXattr(to, attr, value, 0); err != nil {
			return fmt.Errorf("the volume has a POSIX ACL (%s) that could not be applied to %s, "+
				"so a swap would change who can reach the restored data (nothing was changed); "+
				"run the restore as root, or use --merge to write in place: %w", attr, to, err)
		}
	}
	return nil
}

const seLinuxAttr = "security.selinux"

// setXattr is the seam for the one operation this package cannot exercise on a
// non-SELinux kernel. The decision it feeds (an existing label that will not
// apply must stop the restore) is the part worth testing, and it is testable
// without SELinux; whether a copied label is *sufficient* on an enforcing host
// is not, and needs a real one.
var setXattr = unix.Lsetxattr

// removeXattr deletes an extended attribute, treating "it was not there" as
// success: the caller only wants it gone.
var removeXattr = func(path, attr string) error {
	err := unix.Lremovexattr(path, attr)
	if err == nil || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}
	return err
}

// listXattrsFn is the seam for enumerating attributes, so a test can present a
// volume carrying attributes this filesystem cannot actually hold.
var listXattrsFn = listXattrs

// readXattrFn is the matching read seam, so a test can present a label on a
// kernel that has none.
var readXattrFn = readXattr

// readSELinuxContext returns the directory's SELinux label and whether it had
// one at all. A host without SELinux, or a filesystem without extended
// attributes, has nothing to copy and is not an error.
func readSELinuxContext(path string) (label string, present bool, err error) {
	value, present, err := readXattrFn(path, seLinuxAttr)
	if err != nil || !present {
		return "", false, err
	}
	label = strings.TrimRight(string(value), "\x00")
	return label, label != "", nil
}

// readXattr returns an extended attribute and whether it was set at all. The
// size is queried first: a fixed buffer that comes up short fails with ERANGE
// rather than truncating, and SELinux contexts with long MCS category sets do
// overflow the obvious guesses. Absent, or a filesystem without xattr support,
// is normal and not an error.
// statxFn is the seam for the encryption probe, so a test can present a kernel
// that cannot answer without needing one.
var statxFn = unix.Statx

// encryptionBlocksStaging reports whether a target must be restored in place
// rather than staged, because of encryption, and why.
//
// An fscrypt policy is applied when a directory is created and inherited from
// its parent; it is filesystem-internal state, not an extended attribute that
// can be copied onto a sibling afterwards. A staging directory created under an
// unencrypted parent has no policy, so borg writes the restored files in
// plaintext and the swap replaces an encrypted volume with a readable one while
// reporting success.
//
// The two failure directions are not symmetric, and this returns yes whenever
// it cannot prove no. Staging something that turns out to be encrypted is a
// silent confidentiality loss. Restoring in place unnecessarily only gives up
// this PR's safety net, which is exactly where such a host already was.
//
// A statx that answers is trusted in both directions: fscrypt exists only on
// filesystems that report STATX_ATTR_ENCRYPTED in the mask, so a mask without
// that bit means the question does not arise here. A statx that cannot answer
// at all establishes nothing, and old kernels that predate it (fscrypt landed
// before statx did) are exactly where an encrypted directory can exist and go
// unseen.
func encryptionBlocksStaging(path string) (blocks bool, reason string, err error) {
	var st unix.Statx_t
	if statErr := statxFn(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS, &st); statErr != nil {
		if errors.Is(statErr, unix.ENOSYS) || errors.Is(statErr, unix.EOPNOTSUPP) ||
			errors.Is(statErr, unix.ENOTSUP) || errors.Is(statErr, unix.EPERM) {
			return true, "whether this volume's data directory is encrypted cannot be determined on this kernel, " +
				"and staging an encrypted directory would replace it with unencrypted data", nil
		}
		return false, "", fmt.Errorf("inspecting %s: %w", path, statErr)
	}
	if st.Attributes_mask&unix.STATX_ATTR_ENCRYPTED == 0 {
		return false, "", nil // this filesystem has no encryption to report on
	}
	if st.Attributes&unix.STATX_ATTR_ENCRYPTED != 0 {
		return true, "this volume's data directory is encrypted, and an encryption policy cannot be applied to " +
			"the replacement directory a swap would need", nil
	}
	return false, "", nil
}

// btrfsSubvolumeRootInode is the inode number every btrfs subvolume root has.
// It is an unremarkable number on any other filesystem, so it only means this
// once the filesystem is known to be btrfs.
const btrfsSubvolumeRootInode = 256

// createStagingDir makes the directory the restore extracts into, matching what
// the target *is* and not merely what it contains.
//
// The exchange swaps two entries in one parent directory, so a plain staging
// sibling trades places with a btrfs subvolume root perfectly happily: the
// restore succeeds and nothing warns. What it leaves behind is an ordinary
// directory where a subvolume used to be, so the operator's snapshots,
// send/receive, and quota groups silently stop applying to that volume. Created
// as a subvolume, the exchange preserves it.
//
// Created restrictively and widened afterwards: MkdirTemp makes it 0700, and
// umask would mask an intended mode passed to Mkdir anyway.
func createStagingDir(model string, logger *slog.Logger) (string, error) {
	staging, err := os.MkdirTemp(filepath.Dir(model), scratchPattern(model, stagingPrefix))
	if err != nil {
		return "", err
	}
	subvolume, err := isBtrfsSubvolumeRoot(model)
	if err != nil {
		return "", err
	}
	if !subvolume {
		markAsOurs(staging)
		return staging, nil
	}
	// Replace the placeholder with a subvolume of the same name. Safe rather
	// than racy: MkdirTemp has just proved the name free, and the per-volume
	// restore lock means nothing else is restoring this volume.
	if rmErr := os.Remove(staging); rmErr != nil {
		return "", rmErr
	}
	if subErr := createBtrfsSubvolume(staging); subErr != nil {
		// Losing the subvolume is worth saying out loud, but it is not worth
		// refusing a restore over: the data lands correctly either way, and an
		// operator without btrfs-progs installed still needs their volume back.
		logger.Warn("this volume's data directory is a btrfs subvolume, but a subvolume could not be created to "+
			"replace it, so the restored directory will be an ordinary one; snapshots, send/receive, or quotas "+
			"set up against this subvolume will stop applying to it",
			"path", staging, "error", subErr)
		if mkErr := os.Mkdir(staging, 0o700); mkErr != nil {
			return "", mkErr
		}
		markAsOurs(staging)
		return staging, nil
	}
	// A new subvolume has a new subvolume id, so it is a new qgroup. Limits and
	// parent assignments are keyed on that id and stay with the old one, which
	// this restore is about to delete: the volume silently leaves whatever quota
	// it was under. Reproducing the whole qgroup arrangement is more than this
	// can honestly promise, so say what changed and let the operator reapply it.
	if btrfsQuotaEnabled(model) {
		logger.Warn("btrfs quotas are enabled and this volume is a subvolume, so the restore gives it a new "+
			"subvolume id and therefore a new qgroup; any qgroup limit or parent assignment on the old one does "+
			"not follow it and has to be reapplied (btrfs qgroup show -re <mountpoint>)",
			"path", model)
	}
	// #nosec G302 -- a directory, and briefly: it takes the model's mode next
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}
	markAsOurs(staging)
	return staging, nil
}

// matchSubvolumeReadOnly makes the replacement subvolume read-only when the one
// it replaces is.
//
// A read-only subvolume or snapshot is a deliberate protection, and the
// in-place restore could not write through it at all. The staged swap builds a
// writable replacement and promotes it, so an immutable volume quietly becomes
// writable while the restore reports success.
//
// Only ever adds the restriction, never removes it: a writable target gets a
// writable replacement by construction, so there is nothing to clear.
func matchSubvolumeReadOnly(targetData, staging string, logger *slog.Logger) error {
	subvolume, err := isBtrfsSubvolumeRoot(targetData)
	if err != nil || !subvolume {
		return err
	}
	readOnly, known := btrfsSubvolumeReadOnly(targetData)
	if !known || !readOnly {
		return nil
	}
	if setErr := setBtrfsSubvolumeReadOnly(staging, true); setErr != nil {
		logger.Warn("this volume is a read-only btrfs subvolume, but the subvolume replacing it could not be "+
			"made read-only, so the restored volume will be writable; set it back with "+
			"\"btrfs property set -ts <path> ro true\"",
			"path", staging, "error", setErr)
	}
	return nil
}

// btrfsSubvolumeReadOnly reports a subvolume's read-only property, and whether
// it could be established at all.
var btrfsSubvolumeReadOnly = func(path string) (bool, bool) {
	// #nosec G204 -- fixed argv over a path this process derived
	out, err := exec.Command("btrfs", "property", "get", "-ts", path, "ro").Output()
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(out)) == "ro=true", true
}

var setBtrfsSubvolumeReadOnly = func(path string, readOnly bool) error {
	value := "false"
	if readOnly {
		value = "true"
	}
	// #nosec G204 -- fixed argv over a path this process derived
	out, err := exec.Command("btrfs", "property", "set", "-ts", path, "ro", value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs property set ro=%s: %w: %s", value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// btrfsQuotaEnabled reports whether qgroup accounting may apply to the
// filesystem holding path.
//
// Only a listing that says quotas are off counts as a no. Exit status alone
// does not: listing qgroups needs privilege, so an unprivileged restore gets
// "Operation not permitted" and a plain success check would read that as
// "quotas are disabled" and stay silent exactly where it cannot tell. A warning
// nobody needed is a much smaller harm than a volume that has quietly left its
// quota, so anything undetermined counts as a yes.
var btrfsQuotaEnabled = func(path string) bool {
	// #nosec G204 -- fixed argv over a path this process derived
	out, err := exec.Command("btrfs", "qgroup", "show", "-f", path).CombinedOutput()
	if err == nil {
		return true
	}
	return !strings.Contains(string(out), "quotas not enabled")
}

// isBtrfsSubvolumeRoot reports whether path is the root of its own btrfs
// subvolume. Such a root is not a separate mount, so /proc/self/mountinfo does
// not show it and isOwnMountPoint says nothing about it.
func isBtrfsSubvolumeRoot(path string) (bool, error) {
	onBtrfs, err := config.IsBtrfs(path)
	if err != nil {
		return false, fmt.Errorf("checking the filesystem of %s: %w", path, err)
	}
	if !onBtrfs {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return st.Ino == btrfsSubvolumeRootInode, nil
}

// createBtrfsSubvolume shells out because creating one otherwise means issuing
// BTRFS_IOC_SUBVOL_CREATE by hand through unsafe pointers, and this package
// already runs an external binary (cp --reflink) for the snapshot path.
var createBtrfsSubvolume = func(path string) error {
	// #nosec G204 -- fixed argv over a path this process derived
	out, err := exec.Command("btrfs", "subvolume", "create", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs subvolume create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// listXattrs names every extended attribute on path. A filesystem without
// xattr support reports none rather than failing: there is then nothing to
// preserve.
func listXattrs(path string) ([]string, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing the extended attributes on %s: %w", path, err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Llistxattr(path, buf)
	if err != nil {
		return nil, fmt.Errorf("listing the extended attributes on %s: %w", path, err)
	}
	var names []string
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if len(name) > 0 {
			names = append(names, string(name))
		}
	}
	return names, nil
}

// copyRemainingXattrs copies the extended attributes the SELinux and ACL steps
// do not already set from the model.
//
// The in-place restore kept the directory's own inode, so anything an
// application had labelled its volume root with survived by default. Building a
// replacement directory means every attribute not copied here is silently
// dropped, or worse, replaced by whatever the archive happened to carry.
func copyRemainingXattrs(from, to string, logger *slog.Logger) error {
	names, err := listXattrsFn(from)
	if err != nil {
		return err
	}
	for _, attr := range names {
		if explicitlyHandledXattrs[attr] {
			continue
		}
		value, present, readErr := readXattrFn(from, attr)
		if readErr != nil {
			return readErr
		}
		if !present {
			continue
		}
		if setErr := setXattr(to, attr, value, 0); setErr != nil {
			// trusted.* needs CAP_SYS_ADMIN, and a security.* attribute owned by
			// some other LSM may be unwritable from here. Refusing to restore
			// over an attribute the operator likely does not know exists is the
			// worse failure, so name it and carry on.
			if errors.Is(setErr, unix.EPERM) || errors.Is(setErr, unix.EACCES) ||
				errors.Is(setErr, unix.ENOTSUP) || errors.Is(setErr, unix.EOPNOTSUPP) {
				logger.Warn("could not copy an extended attribute from the volume to its replacement, "+
					"so it will be missing after the restore",
					"attribute", attr, "path", to, "error", setErr)
				continue
			}
			return fmt.Errorf("copying the extended attribute %s onto %s (nothing was changed): %w", attr, to, setErr)
		}
	}
	return nil
}

// explicitlyHandledXattrs are set from the model by their own steps, which
// report their own failures in terms an operator can act on. Derived from those
// steps' own lists rather than repeating them, so an attribute added to one
// cannot silently start being written twice.
var explicitlyHandledXattrs = func() map[string]bool {
	// The scratch mark is this tool's own bookkeeping, never the volume's, so a
	// stale one left on a promoted volume must not be copied onward as if it
	// described the data.
	handled := map[string]bool{seLinuxAttr: true, scratchMarkerAttr: true}
	for _, attr := range aclAttrs {
		handled[attr] = true
	}
	return handled
}()

func readXattr(path, attr string) (value []byte, present bool, err error) {
	size, err := unix.Lgetxattr(path, attr, nil)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s on %s: %w", attr, path, err)
	}
	// Present with an empty value, not absent. Absence is ENODATA, handled
	// above. A zero-length user.* attribute is a perfectly ordinary way to mark
	// a directory with a boolean, and reporting it missing dropped it from the
	// replacement silently.
	if size == 0 {
		return nil, true, nil
	}
	buf := make([]byte, size)
	n, err := unix.Lgetxattr(path, attr, buf)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s on %s: %w", attr, path, err)
	}
	return buf[:n], true, nil
}

// applySELinuxContext copies a label the original definitely had. Failing here
// is fatal by design: promoting a mislabelled directory hands the operator a
// restored volume its own container can no longer read, and the wrong moment to
// find that out is after the original has been removed.
func applySELinuxContext(path, label string) error {
	if err := setXattr(path, seLinuxAttr, []byte(label), 0); err != nil {
		return fmt.Errorf("the volume carries the SELinux label %q, which could not be applied to %s, "+
			"so a swap would leave the volume unreadable by its container (nothing was changed); "+
			"run the restore as root, or use --merge to write in place: %w", label, path, err)
	}
	return nil
}

// clearInheritedSELinuxContext removes a label the staging directory was given
// that the volume it replaces does not have.
//
// On a labelled filesystem a fresh directory is assigned a context from policy,
// which the repository's own SELinux test observes happening. The label copying
// only ever applies one the model already carries, and the general xattr copying
// excludes this attribute, so an unlabelled volume could be replaced by a
// labelled root and quietly gain or lose access.
//
// Best effort, and warned about rather than fatal: removing a context is not
// always permitted, and refusing a completed restore over it would be worse
// than reporting that the label differs.
func clearInheritedSELinuxContext(staging string, logger *slog.Logger) error {
	_, present, err := readXattrFn(staging, seLinuxAttr)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if rmErr := removeXattr(staging, seLinuxAttr); rmErr != nil {
		logger.Warn("this volume has no SELinux label but the directory replacing it was given one by policy, "+
			"and it could not be removed, so the restored volume will carry a label the original did not",
			"path", staging, "error", rmErr)
	}
	return nil
}

// syncStagedData flushes the staged restore to disk. A restore is worth little
// if a power loss just after it can bring back a tree that was never written,
// and the copy it replaces is deleted on the strength of this.
//
// It syncs the whole filesystem rather than walking the tree, because the
// contents of an archive are not this process's to open. A regular file
// extracted with mode 000 is readable by root but not by the unprivileged user
// a rootless Podman restore runs as, so opening it to flush it failed with
// EACCES and took a valid restore down with it. A named pipe was worse: opening
// it blocks until a writer appears, and borgmatic has already exited. One
// syncfs needs no descriptor on any of them.
//
// dir must be on the same filesystem as the staged data, which the parent of
// the target satisfies: staging is created as its sibling.
func syncStagedData(dir string) error {
	// #nosec G304 -- the parent of a directory this process is restoring into
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.Syncfs(int(f.Fd()))
}

// syncDirFn is the seam for the durability barrier, so a test can make a flush
// fail without needing a filesystem that will.
var syncDirFn = syncDir

// syncDir flushes a directory entry so renames and creations within it survive
// a power loss.
func syncDir(path string) error {
	// #nosec G304 -- a directory this process is restoring into
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	if closeErr := d.Close(); syncErr == nil {
		syncErr = closeErr
	}
	return syncErr
}

// isOwnMountPoint reports whether path is the root of a mount, which is what a
// volume backed by NFS, CIFS, or a bind looks like. Such a directory cannot be
// renamed (EBUSY) and its staging sibling would land elsewhere, so the
// stage-and-swap strategy does not apply to it.
//
// /proc/self/mountinfo, not a st_dev comparison against the parent: a bind
// mount whose source is on the same filesystem has an identical device number
// and would be missed, and that is exactly how a local-driver bind volume
// looks. Verified: a same-filesystem bind reports st_dev 47 on both sides.
func isOwnMountPoint(path string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", path, err)
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		// Without mountinfo there is nothing to consult. Treat it as not a
		// mount: the swap then fails loudly at the rename rather than silently
		// taking the destructive path.
		return false, nil
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		// Fields: id parent major:minor root mountpoint ...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		// Mount points are octal-escaped for spaces and friends.
		if unescapeMountField(fields[4]) == resolved {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// unescapeMountField decodes the \OOO octal escapes the kernel uses for
// characters that would otherwise break mountinfo's field separation.
func unescapeMountField(field string) string {
	if !strings.Contains(field, "\\") {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if v, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

// retainOrWarn moves a copy that must outlive this run out of the way and
// reports where it ended up, falling back to where it already is.
//
// Both callers need exactly this, and neither can treat a failure as fatal: the
// copy still exists, it is just sitting on a path a later restore reuses, and
// the operator is about to be pointed at it either way. Saying so is the whole
// obligation. It is one function rather than two call sites because leaving that
// warning to be remembered independently is how this went wrong before.
func retainOrWarn(from, targetData string, logger *slog.Logger) string {
	moved, err := retainOutOfTheWay(from, targetData)
	switch {
	case err != nil && moved == "":
		logger.Warn("could not move a copy that has to be kept clear of the staging path; "+
			"a later restore may delete it, so move it yourself if you need it",
			"path", from, "error", err)
		return from
	case err != nil:
		// Moved, but not durably. The path is right and worth reporting; what is
		// not guaranteed is that it survives a power loss before anyone acts.
		logger.Warn("a copy that has to be kept was moved clear of the staging path, but the move "+
			"could not be flushed to disk; copy it somewhere else if you need it to survive a crash",
			"path", moved, "error", err)
		return moved
	}
	return moved
}

// retainOutOfTheWay moves a copy that must outlive this run to a uniquely named
// sibling, away from the staging and displaced paths that later runs reuse.
func retainOutOfTheWay(from, targetData string) (string, error) {
	// The destination is created rather than merely named, because a name is not
	// a claim. Two retentions of the same volume a fraction of a second apart
	// picked the same second-resolution timestamp, and neither outcome was
	// acceptable: renaming onto a non-empty retained copy fails, which drops the
	// caller back to leaving this one on the staging path that the next retry
	// deletes, and renaming onto an empty one succeeds and discards it silently.
	// Widening the timestamp would only narrow that window; MkdirTemp closes it,
	// including against a concurrent process.
	//
	// Renaming onto the empty directory it just made is what gives this its
	// uniqueness, and the retained copy keeps its own mode: the placeholder is
	// replaced, not merged into.
	//
	// unix.Renameat rather than os.Rename, which Lstats the destination and
	// refuses any existing directory, empty or not. The syscall replaces an
	// empty one and refuses a non-empty one, which is exactly the rule wanted
	// here: the placeholder is always ours and always empty, and anything that
	// somehow got into it makes the rename fail rather than discard it.
	stamp := time.Now().Format("20060102-150405")
	kept, err := os.MkdirTemp(filepath.Dir(targetData),
		scratchPattern(targetData, fmt.Sprintf(".borgmatic-manager-kept-%s-", stamp)))
	if err != nil {
		return "", fmt.Errorf("reserving a name to keep %s under: %w", from, err)
	}
	if err := unix.Renameat(unix.AT_FDCWD, from, unix.AT_FDCWD, kept); err != nil {
		err = &os.LinkError{Op: "rename", Old: from, New: kept, Err: err}
		// The placeholder is empty and this restore never used it, so removing
		// it keeps the failure from littering the volumes directory.
		if rmErr := os.Remove(kept); rmErr != nil {
			return "", fmt.Errorf("%w (and the placeholder %s could not be removed: %v)", err, kept, rmErr)
		}
		return "", err
	}
	// The rename has to outlive a power loss, not merely this process. Without
	// this the namespace can roll back to the staging name while the error
	// message still points at the kept one, and the next attempt's opening
	// RemoveAll deletes the completed extract this whole dance existed to save.
	//
	// The path is returned even when the flush fails, because by then the data
	// really is at the new name; the caller warns rather than sending the
	// operator to a path with nothing at it.
	if syncErr := syncDirFn(filepath.Dir(targetData)); syncErr != nil {
		return kept, fmt.Errorf("keeping %s at %s could not be flushed to disk, so it may not survive a power loss: %w",
			from, kept, syncErr)
	}
	return kept, nil
}
