package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// btrfsSubvolInode is the fixed inode number of every btrfs subvolume root.
const btrfsSubvolInode = 256

// btrfsSuperMagic is statfs's f_type for a btrfs filesystem.
const btrfsSuperMagic = 0x9123683E

// IsBtrfs reports whether path resides on a btrfs filesystem.
func IsBtrfs(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, err
	}
	return st.Type == btrfsSuperMagic, nil
}

// IsOwnFilesystemBoundary reports whether path is its own snapshot unit: a
// btrfs subvolume root, or a mount boundary (zfs datasets and LVM logical
// volumes mount separately, so their st_dev differs from the parent's).
func IsOwnFilesystemBoundary(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("no stat data for %s", path)
	}
	if st.Ino == btrfsSubvolInode {
		return true, nil
	}
	parent := filepath.Dir(path)
	pfi, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	pst, ok := pfi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("no stat data for %s", parent)
	}
	return st.Dev != pst.Dev, nil
}

// VolumesRoot maps a volume data path (.../volumes/<name>/_data) to the
// directory whose snapshot unit borgmatic's hooks would actually capture.
func VolumesRoot(hostPath string) string {
	return filepath.Dir(filepath.Dir(hostPath))
}
