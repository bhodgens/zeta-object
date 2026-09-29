//go:build linux

package metadata

import "testing"

// TestZFSSuperMagicConstant pins the corrected ZFS_SUPER_MAGIC value
// (bughunt A1): the historical 0x2f5f2f8b was wrong and made DetectZFS
// always false on Linux, silently disabling the zfs-events provider on
// every ZFS bucket. 0x2fc12fc1 is the statfs f_type the Linux ZFS kernel
// module reports (zfs include/sys/fs/zfs.h; util-linux statfs_magic.h).
func TestZFSSuperMagicConstant(t *testing.T) {
	if zfsSuperMagic != 0x2fc12fc1 {
		t.Fatalf("zfsSuperMagic = %#x, want 0x2fc12fc1 (real ZFS_SUPER_MAGIC)", zfsSuperMagic)
	}
}
