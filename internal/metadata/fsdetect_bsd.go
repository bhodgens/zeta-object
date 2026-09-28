//go:build freebsd || dragonfly

package metadata

import "syscall"

// DetectZFS reports whether the filesystem containing path is ZFS.
// BSD statfs exposes the filesystem type as the f_fstypename string;
// ZFS mounts report "zfs".
func DetectZFS(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, &PathStatError{Path: path, Err: err}
	}
	return fstypename(&st) == "zfs", nil
}

func fstypename(st *syscall.Statfs_t) string {
	b := st.Fstypename[:]
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
