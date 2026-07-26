package config

import (
	"path/filepath"
	"syscall"
)

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

// VolumesRoot maps a volume data path (.../volumes/<name>/_data) to the
// enclosing volumes directory.
func VolumesRoot(hostPath string) string {
	return filepath.Dir(filepath.Dir(hostPath))
}
