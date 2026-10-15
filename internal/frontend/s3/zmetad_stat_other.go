//go:build !linux

package s3

import (
	"fmt"
	"os"
)

// objectIDFromStat stats path and refuses to invent an id.
// ZFS object id equals st_ino only on Linux. A missing file returns the
// Stat error so the caller can answer NoSuchKey. An existing file on
// this OS is an error: tagging must not use a non-ZFS inode.
func objectIDFromStat(path string) (uint64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%w (%s)", errStatIDUnsupported, path)
}
