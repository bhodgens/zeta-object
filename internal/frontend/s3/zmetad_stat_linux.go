//go:build linux

package s3

import (
	"os"
	"syscall"
)

// objectIDFromStat returns the ZFS object id of the file at path.
// On Linux, st_ino is that id: the zmetad CLI filters events with
// stat().st_ino (zfs_main.c). A missing file returns the Stat error
// (os.ErrNotExist). The caller must not guess an id from the event log
// when this fails: a just-written file has no event row yet, and a
// renamed-away path still has a historical row.
func objectIDFromStat(path string) (uint64, error) {
	fi, err := os.Stat(path) //nolint:gosec // G703: path is objectDataPathFor(bucket, validated key), not a request path.
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, os.ErrInvalid
	}
	return uint64(st.Ino), nil
}
