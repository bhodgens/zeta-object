//go:build linux

// reflinkclone_linux.go — the linux arm of the reflink clone op
// (s3-versioning leaf 06): FICLONE via unix.IoctlFileClone. On ZFS 2.2+
// this is a real block-cloned reflink copy (O(1) in bytes); on any
// filesystem/ioctl failure the caller fails soft.
package s3

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reflinkClone block-clones src into the ALREADY-CREATED empty dst file
// (O_EXCL'd by the caller) via FICLONE. The fds are closed by the
// caller; this function owns only the ioctl.
func reflinkClone(dstPath, srcPath string) error {
	src, err := os.Open(srcPath) //nolint:gosec // G304: both paths are store-derived (bucketPath + key-sha + versionId) / sidecar StoragePath, never raw client input.
	if err != nil {
		return fmt.Errorf("s3: opening reflink source %s: %w", srcPath, err)
	}
	defer src.Close() //nolint:errcheck // read-only fd
	dst, err := os.OpenFile(dstPath, os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("s3: opening reflink dest %s: %w", dstPath, err)
	}
	defer dst.Close() //nolint:errcheck // error reported below via the ioctl
	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		return fmt.Errorf("s3: FICLONE %s -> %s: %w", srcPath, dstPath, err)
	}
	return nil
}
