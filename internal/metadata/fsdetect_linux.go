//go:build linux

package metadata

import "syscall"

// zfsSuperMagic is ZFS_SUPER_MAGIC from linux/magic.h.
const zfsSuperMagic = 0x2f5f2f8b

// DetectZFS reports whether the filesystem containing path is ZFS.
// It runs statfs (NOT stat) so it inspects the mounted filesystem, then
// matches the ZFS filesystem magic. The boolean result is the cheap half
// of Probe; leaf 02 adds the dataset/feature check.
func DetectZFS(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, &PathStatError{Path: path, Err: err}
	}
	return st.Type == zfsSuperMagic, nil
}
