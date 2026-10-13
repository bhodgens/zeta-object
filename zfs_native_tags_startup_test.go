package main

// zfs_native_tags_startup_test.go — validateZfsNativeTags tests
// (zfs-metadata#13 consumer leaf). lookPathFn is faked (dev hosts have no
// zmetad on PATH — the same seam convention zfs_startup_test.go uses).
// Covers: feature off = no-op even with everything else broken; the
// fail-loud arms (datasets off, parent unresolved, zmetad binary missing,
// empty db path); and the happy path.

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateZfsNativeTags(t *testing.T) {
	restore := func(t *testing.T, lookPath func(string) (string, error)) {
		t.Helper()
		orig := lookPathFn
		lookPathFn = lookPath
		t.Cleanup(func() { lookPathFn = orig })
	}

	t.Run("feature off is a no-op", func(t *testing.T) {
		zfsBucketsParentDataset = "" // even with everything else unresolved
		if err := validateZfsNativeTags(&ServerConfig{ZfsNativeTags: false}); err != nil {
			t.Fatalf("feature off must not validate: %v", err)
		}
	})

	t.Run("requires zfs_bucket_datasets", func(t *testing.T) {
		zfsBucketsParentDataset = ""
		err := validateZfsNativeTags(&ServerConfig{ZfsNativeTags: true, ZfsBucketDatasets: false})
		if err == nil || !strings.Contains(err.Error(), "zfs_bucket_datasets must be enabled") {
			t.Fatalf("expected datasets-off rejection, got %v", err)
		}
	})

	t.Run("requires a resolved parent", func(t *testing.T) {
		zfsBucketsParentDataset = ""
		err := validateZfsNativeTags(&ServerConfig{
			ZfsNativeTags: true, ZfsBucketDatasets: true, ZmetadBinary: "zmetad", ZmetadDBPath: "/db",
		})
		if err == nil || !strings.Contains(err.Error(), "no parent dataset resolved") {
			t.Fatalf("expected parent-unresolved rejection, got %v", err)
		}
	})

	t.Run("missing zmetad binary aborts", func(t *testing.T) {
		restore(t, func(string) (string, error) { return "", errors.New("not found") })
		zfsBucketsParentDataset = "testpool/zval"
		err := validateZfsNativeTags(&ServerConfig{
			ZfsNativeTags: true, ZfsBucketDatasets: true, ZmetadBinary: "no-such-zmetad", ZmetadDBPath: "/db",
		})
		if err == nil || !strings.Contains(err.Error(), "not found on PATH") {
			t.Fatalf("expected missing-binary rejection, got %v", err)
		}
	})

	t.Run("happy path", func(t *testing.T) {
		restore(t, func(p string) (string, error) { return "/usr/bin/" + p, nil })
		zfsBucketsParentDataset = "testpool/zval"
		if err := validateZfsNativeTags(&ServerConfig{
			ZfsNativeTags: true, ZfsBucketDatasets: true, ZmetadBinary: "zmetad", ZmetadDBPath: "/var/db/zmetad.db",
		}); err != nil {
			t.Fatalf("happy path: %v", err)
		}
	})
}
