package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// stagingSuffix names the sibling directory a mirror restore extracts into
// before it replaces the live one. Sibling, not a subdirectory of the target:
// it has to be on the same filesystem for the swap to be a rename, and inside
// the target it would be part of what the restore is meant to replace.
const stagingSuffix = ".borgmatic-manager-restoring"

// oldSuffix names the displaced original between the two renames. It only
// exists inside swapIntoPlace, and is removed once the new data is live.
const oldSuffix = ".borgmatic-manager-replaced"

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
func restoreWithSwap(targetData string, logger *slog.Logger, extract func(destination string) error) error {
	staging := targetData + stagingSuffix

	// A previous run killed between extract and swap leaves this behind. It is
	// never live data (the swap renames rather than copies), so it is safe to
	// discard, and reusing it would mix two restores together.
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clearing a leftover staging directory %s: %w", staging, err)
	}

	if err := createStagingLike(staging, targetData); err != nil {
		return err
	}
	// Any failure from here on discards the staging directory and leaves the
	// live data exactly as it was.
	committed := false
	defer func() {
		if !committed {
			if err := os.RemoveAll(staging); err != nil {
				logger.Warn("could not remove the staging directory; the restored data is there and the volume is untouched",
					"staging", staging, "error", err)
			}
		}
	}()

	if err := extract(staging); err != nil {
		return fmt.Errorf("extracting into %s (the volume is untouched): %w", staging, err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("reading back the extracted data (the volume is untouched): %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("borgmatic reported success but extracted nothing into %s, so the volume is untouched", staging)
	}

	displaced, err := swapIntoPlace(staging, targetData)
	if err != nil {
		return err
	}
	committed = true
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
// displaced original now sits at, for the caller to dispose of. The rename pair
// is not one atomic step, so the window between them is kept to exactly two
// syscalls, and a failure in the second puts the original back rather than
// leaving the volume without a data directory at all.
func swapIntoPlace(staging, targetData string) (displaced string, err error) {
	displaced = targetData + oldSuffix
	if err := os.RemoveAll(displaced); err != nil {
		return "", fmt.Errorf("clearing %s before the swap: %w", displaced, err)
	}
	if err := os.Rename(targetData, displaced); err != nil {
		return "", fmt.Errorf("moving the current data aside (the volume is untouched): %w", err)
	}
	if err := os.Rename(staging, targetData); err != nil {
		// Put it back: a volume with no data directory is worse than a failed
		// restore, and this is the only moment that state can exist.
		if back := os.Rename(displaced, targetData); back != nil {
			return "", fmt.Errorf("restore failed mid-swap and the original could not be put back: "+
				"the data is at %s, move it to %s by hand: %w", displaced, targetData, errors.Join(err, back))
		}
		return "", fmt.Errorf("moving the restored data into place (the volume is unchanged): %w", err)
	}
	return displaced, nil
}

// createStagingLike makes the staging directory carry the same identity as the
// directory it will replace: owner, mode, and SELinux context. A fresh
// directory would otherwise get the creating process's defaults, and on an
// SELinux host a volume relabelled that way stops being readable by the
// container that owns it.
func createStagingLike(staging, model string) error {
	info, err := os.Stat(model)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", model, err)
	}
	if err := os.Mkdir(staging, info.Mode().Perm()); err != nil {
		return fmt.Errorf("creating the staging directory %s: %w", staging, err)
	}
	// Mkdir's mode is masked by umask; set it exactly.
	if err := os.Chmod(staging, info.Mode().Perm()); err != nil {
		return fmt.Errorf("setting the mode on %s: %w", staging, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		if err := os.Chown(staging, int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("setting the owner on %s: %w", staging, err)
		}
	}
	if err := copySELinuxContext(model, staging); err != nil {
		return err
	}
	return nil
}

// copySELinuxContext mirrors the security.selinux label. A host without
// SELinux, or a filesystem without extended attributes, has nothing to copy
// and is not an error.
func copySELinuxContext(from, to string) error {
	const attr = "security.selinux"
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(from, attr, buf)
	if err != nil {
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil // not an SELinux host, or no xattr support
		}
		return fmt.Errorf("reading the SELinux context of %s: %w", from, err)
	}
	label := strings.TrimRight(string(buf[:n]), "\x00")
	if label == "" {
		return nil
	}
	if err := unix.Lsetxattr(to, attr, []byte(label), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			return nil // cannot label here; the swap is still correct
		}
		return fmt.Errorf("applying the SELinux context %q to %s: %w", label, to, err)
	}
	return nil
}

// stagingPathFor is the staging sibling for a target, exposed so callers can
// name it in messages without repeating the suffix.
func stagingPathFor(targetData string) string { return targetData + stagingSuffix }
