package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// stagingSuffix names the sibling directory a mirror restore extracts into
// before it replaces the live one. Sibling, not a subdirectory of the target:
// it has to be on the same filesystem for the swap to be a rename, and inside
// the target it would be part of what the restore is meant to replace.
const stagingSuffix = ".borgmatic-manager-restoring"

// oldSuffix names the displaced original on the fallback swap path. It only
// exists between two renames, and only where RENAME_EXCHANGE is unavailable.
const oldSuffix = ".borgmatic-manager-replaced"

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
func restoreWithSwap(targetData string, logger *slog.Logger, allowEmpty bool, extract func(destination string) error) (err error) {
	staging := targetData + stagingSuffix

	// A previous run killed between extract and swap leaves this behind. It is
	// never live data (the swap renames rather than copies), so it is safe to
	// discard, and reusing it would mix two restores together.
	if rmErr := os.RemoveAll(staging); rmErr != nil {
		return fmt.Errorf("clearing a leftover staging directory %s: %w", staging, rmErr)
	}

	if createErr := createStagingLike(staging, targetData); createErr != nil {
		return createErr
	}
	// Any failure before the swap discards the staging directory and leaves the
	// live data exactly as it was. The one exception is a stranded swap, where
	// staging holds the only copy of the restored data.
	committed := false
	defer func() {
		if committed || errors.Is(err, errStranded) {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			logger.Warn("could not remove the staging directory; the volume itself is untouched",
				"staging", staging, "error", rmErr)
		}
	}()

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
	if syncErr := syncTree(staging); syncErr != nil {
		return fmt.Errorf("flushing the restored data to disk (the volume is untouched): %w", syncErr)
	}

	displaced, swapErr := swapIntoPlace(staging, targetData)
	if swapErr != nil {
		return swapErr
	}
	committed = true
	// Persist the swap itself before removing the copy it replaced, or a power
	// loss here could replay the deletion without the rename.
	if syncErr := syncDir(filepath.Dir(targetData)); syncErr != nil {
		logger.Warn("could not flush the volume directory after the swap; the restore is in place but not yet durable",
			"path", filepath.Dir(targetData), "error", syncErr)
	}
	// The restore is done and live. Failing to delete the copy it replaced is
	// wasted disk, not a failed restore, so it must not become a nonzero exit
	// for an operation that succeeded.
	if rmErr := os.RemoveAll(displaced); rmErr != nil {
		logger.Warn("restore succeeded, but the data it replaced could not be removed; delete it to reclaim the space",
			"path", displaced, "error", rmErr)
	}
	logger.Info("restore complete", "path", targetData, "entries", len(entries))
	return nil
}

// swapIntoPlace makes staging the live data directory and returns the path the
// displaced original ended up at, for the caller to dispose of.
//
// RENAME_EXCHANGE trades the two directories in one atomic step, so there is no
// instant at which the volume has no data directory, even across power loss.
// Filesystems without it fall back to a pair of renames, which leaves a window
// of exactly two syscalls; recoverInterruptedSwap repairs that window if the
// process dies inside it.
func swapIntoPlace(staging, targetData string) (displaced string, err error) {
	switch exErr := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, targetData, unix.RENAME_EXCHANGE); {
	case exErr == nil:
		// The directories traded places: the old data is now under the staging
		// name, which is what the caller removes.
		return staging, nil
	case errors.Is(exErr, unix.ENOSYS), errors.Is(exErr, unix.EINVAL),
		errors.Is(exErr, unix.EOPNOTSUPP), errors.Is(exErr, unix.ENOTSUP):
		// No atomic exchange on this kernel or filesystem; use the rename pair.
	default:
		return "", fmt.Errorf("swapping the restored data into place (the volume is untouched): %w", exErr)
	}

	return swapByRenamePair(staging, targetData)
}

// swapByRenamePair is the swap for filesystems without RENAME_EXCHANGE. It is
// separate so the rollback below can be exercised directly: on a kernel that
// does support the atomic exchange, nothing else can reach this code.
func swapByRenamePair(staging, targetData string) (displaced string, err error) {
	displaced = targetData + oldSuffix
	if rmErr := os.RemoveAll(displaced); rmErr != nil {
		return "", fmt.Errorf("clearing %s before the swap: %w", displaced, rmErr)
	}
	if renErr := os.Rename(targetData, displaced); renErr != nil {
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
		return "", fmt.Errorf("moving the restored data into place (the volume is unchanged): %w", renErr)
	}
	return displaced, nil
}

// recoverInterruptedSwap repairs a volume whose data directory is missing but
// whose displaced original is still on disk: the state left if the process dies
// between the fallback path's two renames. Without this the next restore stops
// at "data directory is not present" and the operator has to know to go looking
// for a suffixed directory.
func recoverInterruptedSwap(targetData string, logger *slog.Logger) error {
	if _, err := os.Lstat(targetData); err == nil || !os.IsNotExist(err) {
		return nil // present, or unreadable for some other reason: nothing to do
	}
	displaced := targetData + oldSuffix
	if _, err := os.Lstat(displaced); err != nil {
		return nil // nothing to recover from
	}
	if err := os.Rename(displaced, targetData); err != nil {
		return fmt.Errorf("a previous restore was interrupted mid-swap and %s could not be moved back to %s: %w",
			displaced, targetData, err)
	}
	logger.Warn("a previous restore was interrupted mid-swap; the volume's data has been put back",
		"path", targetData, "recovered_from", displaced)
	return nil
}

// createStagingLike makes the staging directory carry the same identity as the
// directory it will replace: owner, mode including the special bits, and
// SELinux context. A fresh directory would otherwise get the creating process's
// defaults, and on an SELinux host a volume relabelled that way stops being
// readable by the container that owns it.
func createStagingLike(staging, model string) error {
	info, err := os.Stat(model)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", model, err)
	}
	// Read the label before creating anything: an unreadable one is a reason to
	// stop, not something to discover after the directory exists.
	label, labelled, err := readSELinuxContext(model)
	if err != nil {
		return err
	}

	// Created restrictively and widened afterwards: it briefly exists at its
	// final path, and umask would mask an intended mode passed to Mkdir anyway.
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("creating the staging directory %s: %w", staging, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		if err := os.Chown(staging, int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("setting the owner on %s: %w", staging, err)
		}
	}
	// After Chown, which clears setuid and setgid.
	mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if err := os.Chmod(staging, mode); err != nil {
		return fmt.Errorf("setting the mode on %s: %w", staging, err)
	}
	if labelled {
		if err := applySELinuxContext(staging, label); err != nil {
			return err
		}
	}
	return copyAccessControlLists(model, staging)
}

// aclAttrs are the extended attributes POSIX ACLs live in. Dropping them on the
// replacement can revoke access the volume's users had, and the default ACL
// also governs what files created after the restore inherit, so they are copied
// with the same "an existing one must apply" rule as the SELinux label.
var aclAttrs = []string{"system.posix_acl_access", "system.posix_acl_default"}

func copyAccessControlLists(from, to string) error {
	for _, attr := range aclAttrs {
		value, present, err := readXattr(from, attr)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := unix.Lsetxattr(to, attr, value, 0); err != nil {
			return fmt.Errorf("the volume has a POSIX ACL (%s) that could not be applied to %s, "+
				"so a swap would change who can reach the restored data (nothing was changed); "+
				"run the restore as root, or use --merge to write in place: %w", attr, to, err)
		}
	}
	return nil
}

const seLinuxAttr = "security.selinux"

// readSELinuxContext returns the directory's SELinux label and whether it had
// one at all. A host without SELinux, or a filesystem without extended
// attributes, has nothing to copy and is not an error.
func readSELinuxContext(path string) (label string, present bool, err error) {
	value, present, err := readXattr(path, seLinuxAttr)
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
func readXattr(path, attr string) (value []byte, present bool, err error) {
	size, err := unix.Lgetxattr(path, attr, nil)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s on %s: %w", attr, path, err)
	}
	if size == 0 {
		return nil, false, nil
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
	if err := unix.Lsetxattr(path, seLinuxAttr, []byte(label), 0); err != nil {
		return fmt.Errorf("the volume carries the SELinux label %q, which could not be applied to %s, "+
			"so a swap would leave the volume unreadable by its container (nothing was changed); "+
			"run the restore as root, or use --merge to write in place: %w", label, path, err)
	}
	return nil
}

// syncTree flushes every file and directory under root, then root itself. A
// restore is worth little if a power loss just after it can bring back a tree
// that was never written, so this is deliberately thorough rather than cheap.
//
// Every open goes through os.Root, which resolves relative to the staging
// directory and refuses to escape it. The tree is extracted from an archive, so
// its shape is not this program's to trust: a path that is a directory when
// walked and a symlink by the time it is opened would otherwise follow it out.
func syncTree(root string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// A symlink has no contents of its own to flush.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		f, openErr := r.Open(rel)
		if openErr != nil {
			return openErr
		}
		syncErr := f.Sync()
		if closeErr := f.Close(); syncErr == nil {
			syncErr = closeErr
		}
		return syncErr
	})
}

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

// stagingPathFor is the staging sibling for a target, exposed so callers can
// name it in messages without repeating the suffix.
func stagingPathFor(targetData string) string { return targetData + stagingSuffix }
