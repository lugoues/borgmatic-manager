package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// stillSafe is checked again immediately before the swap. The extract can take
// a long time, and a container started meanwhile would have mounted the very
// inode the swap is about to move out from under it.
func restoreWithSwap(targetData string, logger *slog.Logger, allowEmpty bool, extract func(destination string) error, stillSafe func() error) (err error) {
	staging := targetData + stagingSuffix

	// A previous run killed between extract and swap leaves this behind. It is
	// never live data (the swap renames rather than copies), so it is safe to
	// discard, and reusing it would mix two restores together.
	if rmErr := os.RemoveAll(staging); rmErr != nil {
		return fmt.Errorf("clearing a leftover staging directory %s: %w", staging, rmErr)
	}

	// Registered before the directory exists, so a createStagingLike that fails
	// partway through its owner/mode/label work does not leave a half-built
	// directory behind. RemoveAll on a path that was never created is a no-op.
	//
	// Any failure before the swap discards the staging directory and leaves the
	// live data exactly as it was. The exceptions are a stranded swap and an
	// aborted recheck, where staging holds the only copy of the restored data.
	committed, keepStaging := false, false
	defer func() {
		if committed || keepStaging || errors.Is(err, errStranded) {
			return
		}
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			logger.Warn("could not remove the staging directory; the volume itself is untouched",
				"staging", staging, "error", rmErr)
		}
	}()

	if createErr := createStagingLike(staging, targetData, logger); createErr != nil {
		return createErr
	}

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

	displaced, swapErr := swapIntoPlace(staging, targetData)
	if swapErr != nil {
		return swapErr
	}
	committed = true
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
	if resolved, err := filepath.EvalSymlinks(targetData); err == nil {
		return resolved, nil
	}
	info, err := os.Lstat(targetData)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		// Not a symlink, so there is nothing here to resolve. Whatever is wrong
		// with the path is reported by the caller's own existence check, which
		// can say more about it than this function can.
		return targetData, nil
	}
	link, err := os.Readlink(targetData)
	if err != nil {
		return "", fmt.Errorf("reading the symlink at %s: %w", targetData, err)
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(targetData), link)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(link))
	if err != nil {
		return "", fmt.Errorf("%s points into %s, which could not be resolved: %w", targetData, filepath.Dir(link), err)
	}
	return filepath.Join(parent, filepath.Base(link)), nil
}

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
func createStagingLike(staging, model string, logger *slog.Logger) error {
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
	if err := copyAccessControlLists(model, staging); err != nil {
		return err
	}
	return copyRemainingXattrs(model, staging, logger)
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
	handled := map[string]bool{seLinuxAttr: true}
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
	if err := setXattr(path, seLinuxAttr, []byte(label), 0); err != nil {
		return fmt.Errorf("the volume carries the SELinux label %q, which could not be applied to %s, "+
			"so a swap would leave the volume unreadable by its container (nothing was changed); "+
			"run the restore as root, or use --merge to write in place: %w", label, path, err)
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
	if err != nil {
		logger.Warn("could not move a copy that has to be kept clear of the staging path; "+
			"a later restore may delete it, so move it yourself if you need it",
			"path", from, "error", err)
		return from
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
		fmt.Sprintf("%s.borgmatic-manager-kept-%s-", filepath.Base(targetData), stamp))
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
	return kept, nil
}

// stagingPathFor is the staging sibling for a target, exposed so callers can
// name it in messages without repeating the suffix.
func stagingPathFor(targetData string) string { return targetData + stagingSuffix }
