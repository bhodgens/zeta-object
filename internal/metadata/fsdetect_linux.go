//go:build linux

package metadata

import "syscall"

// zfsSuperMagic is ZFS_SUPER_MAGIC — the statfs(2) f_type value the Linux
// ZFS kernel module reports for ZFS mounts (zfs include/sys/fs/zfs.h;
// util-linux include/statfs_magic.h STATFS_ZFS_MAGIC). The historical
// value here (0x2f5f2f8b) was wrong and made DetectZFS always false on
// Linux, so every ZFS bucket silently probed unavailable.
const zfsSuperMagic = 0x2fc12fc1

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
