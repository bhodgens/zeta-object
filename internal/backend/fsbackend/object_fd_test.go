// object_fd_test.go — Stat/Owner leak no file descriptor.
//
// statLocked is the shared Get/Stat core: it opens the data file INSIDE the
// reader-lock region (bughunt B3 — the open must not move outside it or a
// concurrent delete's empty-dir prune wins a stat→open race). Get hands the
// opened *os.File to its caller; Stat and Owner discarded it, so every call
// leaked one descriptor until the process ran out (any caller doing a
// Stat-per-item operation — a batch delete, a List-then-stat sweep — inherited
// it).
//
// The fd count is read by ENUMERATING the process's own descriptor
// directory: /dev/fd on darwin, /proc/self/fd on linux (CI). Both are
// available here; the helper skips only when neither exists. Go closes the
// enumeration's own descriptor before returning, so it does not perturb the
// count (verified: a 20-open/20-close round trip returns to the baseline).
package fsbackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// countOpenFDs enumerates this process's open descriptors. -1 means the
// platform exposes neither /dev/fd nor /proc/self/fd, in which case the
// caller skips rather than passing vacuously.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/dev/fd", "/proc/self/fd"} {
		ents, err := os.ReadDir(dir)
		if err == nil {
			return len(ents)
		}
	}
	return -1
}

// assertNoFDGrowth runs body n times and fails if the open-descriptor count
// grows by more than the tolerated noise. A per-call leak shows up as growth
// proportional to n (200 Stats leaked exactly 200 descriptors).
func assertNoFDGrowth(t *testing.T, n int, body func(i int)) {
	t.Helper()
	if countOpenFDs(t) < 0 {
		t.Skip("no descriptor enumeration on this platform (/dev/fd and /proc/self/fd both absent)")
	}
	// Warm-up round: one-time runtime descriptors (epoll, kqueue, the first
	// stat of a lazily-opened dir) must not be charged to the measured round.
	for i := range 3 {
		body(i)
	}
	before := countOpenFDs(t)
	for i := range n {
		body(i)
	}
	after := countOpenFDs(t)
	// Tolerance of a couple of descriptors covers descriptors the Go runtime
	// may open concurrently (a timer/logging fd); a per-call leak is n.
	if growth := after - before; growth > 2 {
		t.Fatalf("open descriptors grew by %d over %d calls (before %d, after %d): the data file opened per call is never closed",
			growth, n, before, after)
	}
}

// TestStat_NoFDLeak is the pin: N Stats of an existing key must not grow the
// descriptor table. Before the fix every call leaked the *os.File statLocked
// opened under the lock region.
func TestStat_NoFDLeak(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "a/b", "data")
	assertNoFDGrowth(t, 200, func(i int) {
		if _, err := f.Stat(ctx, "bkt", "a/b"); err != nil {
			t.Fatalf("Stat call %d: %v", i, err)
		}
	})
}

// TestOwner_NoFDLeak pins the same property for the third statLocked caller:
// the s3 frontend's principal enrichment (xattr.go) reads the owner xattr
// through statLocked and had the identical discard-the-reader leak, so
// serving ?events on a large prefix leaked a descriptor per object.
func TestOwner_NoFDLeak(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, f, "bkt", "a/b", "data")
	assertNoFDGrowth(t, 200, func(i int) {
		f.Owner("bkt", "a/b")
	})
}

// TestStat_ErrorPathsNoFDLeak pins the not-found paths: they return BEFORE
// statLocked opens the data file, so they never leaked and must keep not
// leaking (a fix that opened earlier would regress this).
func TestStat_ErrorPathsNoFDLeak(t *testing.T) {
	root := t.TempDir()
	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "a/b", "data")
	// ENOTDIR class: a FILE shadows the "a" directory the key lives under
	// (bughunt B7) — the data stat fails before any open.
	bucketDir := filepath.Join(root, "bkt")
	if err := os.RemoveAll(filepath.Join(bucketDir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "a"), []byte("prefix-file"), 0644); err != nil {
		t.Fatal(err)
	}
	assertNoFDGrowth(t, 100, func(i int) {
		if _, err := f.Stat(ctx, "bkt", "a/b"); err == nil {
			t.Fatalf("call %d: Stat ENOTDIR = nil error, want NoSuchKey", i)
		}
	})
	assertNoFDGrowth(t, 100, func(i int) {
		if _, err := f.Stat(ctx, "bkt", "missing-key"); err == nil {
			t.Fatalf("call %d: Stat missing key = nil error, want NoSuchKey", i)
		}
	})
	assertNoFDGrowth(t, 100, func(i int) {
		if _, err := f.Stat(ctx, "no-such-bucket", "k"); err == nil {
			t.Fatalf("call %d: Stat missing bucket = nil error, want NoSuchBucket", i)
		}
	})
}

// TestStat_EdgePathBehaviorUnchanged pins the two edge shapes the
// descriptor-close fix must NOT disturb: a missing key, and a DIRECTORY
// occupying the key's data path (Stat stats and opens it, so it answers with
// the directory's size — pre-existing behavior, deliberately preserved here
// rather than silently turned into a NoSuchKey in a leak fix).
func TestStat_EdgePathBehaviorUnchanged(t *testing.T) {
	root := t.TempDir()
	f, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Create the bucket so a missing-KEY lookup is not a missing-BUCKET one.
	mustPut(t, f, "bkt", "seed", "x")

	t.Run("missing key is NoSuchKey", func(t *testing.T) {
		_, err := f.Stat(ctx, "bkt", "nope")
		var omErr *objectmodel.Error
		if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
			t.Fatalf("Stat missing key = %v, want code NoSuchKey", err)
		}
	})

	t.Run("missing bucket is NoSuchBucket", func(t *testing.T) {
		_, err := f.Stat(ctx, "no-such-bucket", "k")
		var omErr *objectmodel.Error
		if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchBucket {
			t.Fatalf("Stat missing bucket = %v, want code NoSuchBucket", err)
		}
	})

	t.Run("directory at the data path still stats", func(t *testing.T) {
		bucketDir := filepath.Join(root, "bkt")
		dirPath := filepath.Join(bucketDir, "d")
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			t.Fatal(err)
		}
		// A hand-written sidecar pointing the data path at the directory —
		// exactly what statLocked reads.
		metaJSON := `{"content_length":0,"etag":"d","last_modified":"2020-01-01T00:00:00Z","storage_path":"` + dirPath + `"}`
		if err := os.MkdirAll(filepath.Join(bucketDir, ".metadata"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bucketDir, ".metadata", "d.meta"), []byte(metaJSON), 0644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dirPath)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := f.Stat(ctx, "bkt", "d")
		if err != nil {
			t.Fatalf("Stat on a directory data path = %v, want the directory's stats (pre-existing behavior)", err)
		}
		if obj.Key != "d" {
			t.Fatalf("Key = %q, want %q", obj.Key, "d")
		}
		// The ACTUAL size is served (sidecar ContentLength is 0).
		if obj.Size != info.Size() {
			t.Fatalf("Size = %d, want the directory's real size %d", obj.Size, info.Size())
		}
		// A Stat that answered from a still-open descriptor would leave the
		// descriptor to leak; this call must be clean too.
		assertNoFDGrowth(t, 50, func(i int) {
			if _, err := f.Stat(ctx, "bkt", "d"); err != nil {
				t.Fatalf("repeat %d: %v", i, err)
			}
		})
	})

	t.Run("directory with no sidecar is NoSuchKey", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "bkt", "lonely"), 0755); err != nil {
			t.Fatal(err)
		}
		_, err := f.Stat(ctx, "bkt", "lonely")
		var omErr *objectmodel.Error
		if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
			t.Fatalf("Stat on a sidecar-less directory = %v, want code NoSuchKey", err)
		}
	})
}

// TestGet_StillReturnsOpenReader is the other half: closing the descriptor
// Stat/Owner discard must NOT close the reader Get hands back. A fix that
// closed inside statLocked would break every streaming read.
func TestGet_StillReturnsOpenReader(t *testing.T) {
	f, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustPut(t, f, "bkt", "k", "hello world")
	rc, _, err := f.Get(ctx, "bkt", "k", objectmodel.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, 5)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("Read from the returned reader: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("read %q, want %q", buf, "hello")
	}
}
