package s3

// zfsdatasets_handlers_test.go — leaf 03 (zfs-bucket-datasets) handler
// wiring tests. These pin the Contract 3 behavior table from
// docs/plans/zfs-bucket-datasets-2026-10/master.md: with the hooks nil
// the legacy plain-dir path runs unchanged; with the hooks installed
// CreateBucket provisions a dataset and DeleteBucket destroys it, with
// the 409 BucketHasSnapshots mapping as the only non-500 hook-error
// wire result. Every test restores the previous hooks in cleanup.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installTestHooks saves the current hook vars, replaces them with the
// given fakes, and restores the previous hooks on cleanup. The create
// fake MkdirAlls the bucket directory to simulate `zfs create`
// producing the mountpoint (leaf spec note: the fake must create the
// real dir or the subsequent .metadata mkdir fails spuriously).
func installTestHooks(
	t *testing.T,
	create func(ctx context.Context, bucket string) (string, error),
	destroy func(ctx context.Context, dataset string) error,
	exists func(ctx context.Context, dataset string) (bool, error),
) {
	t.Helper()
	origCreate, origDestroy, origExists := zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists
	origParent := zfsBucketDatasetParent
	zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists = create, destroy, exists
	SetZfsBucketDatasetParent("pool/data")
	t.Cleanup(func() {
		zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists = origCreate, origDestroy, origExists
		zfsBucketDatasetParent = origParent
	})
}

// callCreateBucket drives createBucketHandler and returns the recorder.
func callCreateBucket(t *testing.T, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket, nil)
	createBucketHandler(w, req, bucket)
	return w
}

// callDeleteBucket drives deleteBucketHandler and returns the recorder.
func callDeleteBucket(t *testing.T, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/"+bucket, nil)
	deleteBucketHandler(w, req, bucket)
	return w
}

// featureOffUsesLegacyMkdirPath pins the hooks-nil row: byte-identical
// legacy behavior — 200, dir + .metadata created.
func TestCreateBucketZfsDataset_FeatureOffLegacyPath(t *testing.T) {
	env := setupS3TestEnv(t)
	w := callCreateBucket(t, "legacy-bucket")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	for _, p := range []string{
		filepath.Join(env.dataDir, "legacy-bucket"),
		filepath.Join(env.dataDir, "legacy-bucket", ".metadata"),
	} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Fatalf("legacy path: %s missing: %v", p, err)
		}
	}
}

// TestCreateBucketZfsDataset_Success: hooks installed, create succeeds
// → 200, hook received the bucket name, .metadata exists under the
// fake-created dir. The create hook being invoked is itself the proof
// the dataset branch ran instead of the legacy MkdirAll path.
func TestCreateBucketZfsDataset_Success(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "zds-create-ok"
	var gotBucket string

	installTestHooks(t,
		func(_ context.Context, b string) (string, error) {
			gotBucket = b
			if err := os.MkdirAll(filepath.Join(env.dataDir, b), 0o755); err != nil { // simulate the zfs mountpoint
				return "", err
			}
			return "pool/data/" + b, nil
		},
		nil,
		nil,
	)

	w := callCreateBucket(t, bucket)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotBucket != bucket {
		t.Fatalf("create hook got bucket %q, want %q", gotBucket, bucket)
	}
	if fi, err := os.Stat(filepath.Join(env.dataDir, bucket, ".metadata")); err != nil || !fi.IsDir() {
		t.Fatalf(".metadata missing after dataset create: %v", err)
	}
}

// TestCreateBucketZfsDataset_HookErrorNoFallback: create hook error →
// 500 InternalError; NO legacy mkdir fallback (the legacy path would
// have left a directory behind — its absence proves it never ran); no
// partial dir.
func TestCreateBucketZfsDataset_HookErrorNoFallback(t *testing.T) {
	env := setupS3TestEnv(t)

	installTestHooks(t,
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("zfs create failed: pool is full")
		},
		nil, nil,
	)

	w := callCreateBucket(t, "zds-create-fail")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "InternalError") {
		t.Fatalf("body missing InternalError code: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-create-fail")); !os.IsNotExist(err) {
		t.Fatal("mkdir fallback ran after create hook failure (dir exists) — contract violation")
	}
}

// TestCreateBucketZfsDataset_MetadataRollback: dataset OK but the
// .metadata mkdir fails (the fake create simulates a mountpoint whose
// parent is read-only, so the handler's os.Mkdir of .metadata fails) →
// rollback destroy of the created dataset, then 500.
func TestCreateBucketZfsDataset_MetadataRollback(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "zds-meta-rollback"
	var destroyed string

	installTestHooks(t,
		func(_ context.Context, b string) (string, error) {
			// Simulate `zfs create` WITHOUT a usable mountpoint: the
			// bucket dir does not appear, so the .metadata mkdir fails.
			return "pool/data/" + b, nil
		},
		func(_ context.Context, dataset string) error {
			destroyed = dataset
			return nil
		},
		nil,
	)

	w := callCreateBucket(t, bucket)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if destroyed != "pool/data/"+bucket {
		t.Fatalf("rollback destroy got dataset %q, want pool/data/%s", destroyed, bucket)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, bucket)); !os.IsNotExist(err) {
		t.Fatal("rollback should leave no bucket dir behind")
	}
}

// TestCreateBucketZfsDataset_RollbackSnapshotsRace: the rollback destroy
// hits ErrDatasetHasSnapshots (host snapshot cron raced the create) →
// STILL 500, dataset left in place (never -r), loud log.
func TestCreateBucketZfsDataset_RollbackSnapshotsRace(t *testing.T) {
	env := setupS3TestEnv(t)
	bucket := "zds-rollback-race"
	destroyErr := fmt.Errorf("s3: destroy pool/data/%s refused: 2 snapshot(s): %w", bucket, ErrDatasetHasSnapshots)

	installTestHooks(t,
		func(_ context.Context, b string) (string, error) {
			// Dataset created without a usable mountpoint → the handler's
			// .metadata mkdir fails → rollback fires.
			return "pool/data/" + b, nil
		},
		func(_ context.Context, _ string) error {
			return destroyErr // snapshots appeared mid-rollback; dataset stays
		},
		nil,
	)

	w := callCreateBucket(t, bucket)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 even when rollback hits snapshots, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, bucket)); !os.IsNotExist(err) {
		t.Fatalf("rollback destroy was refused — the dataset must be LEFT in place: %v", err)
	}
}

// TestCreateBucketZfsDataset_ExistingDirGuard: a pre-existing dir hits
// the existing 409 BucketAlreadyOwnedByYou path and the hooks are never
// called.
func TestCreateBucketZfsDataset_ExistingDirGuard(t *testing.T) {
	env := setupS3TestEnv(t)
	if err := os.MkdirAll(filepath.Join(env.dataDir, "zds-exists"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookCalled := false
	installTestHooks(t,
		func(_ context.Context, _ string) (string, error) { hookCalled = true; return "", nil },
		nil, nil,
	)

	w := callCreateBucket(t, "zds-exists")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketAlreadyOwnedByYou") {
		t.Fatalf("expected BucketAlreadyOwnedByYou, got: %s", w.Body.String())
	}
	if hookCalled {
		t.Fatal("create hook called on the existing-dir guard path")
	}
}

// TestCreateBucketZfsDataset_ExistingFileGuard: a FILE at the bucket
// path → 409 BucketAlreadyExists, hooks not called.
func TestCreateBucketZfsDataset_ExistingFileGuard(t *testing.T) {
	env := setupS3TestEnv(t)
	if err := os.WriteFile(filepath.Join(env.dataDir, "zds-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hookCalled := false
	installTestHooks(t,
		func(_ context.Context, _ string) (string, error) { hookCalled = true; return "", nil },
		nil, nil,
	)

	w := callCreateBucket(t, "zds-file")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketAlreadyExists") {
		t.Fatalf("expected BucketAlreadyExists, got: %s", w.Body.String())
	}
	if hookCalled {
		t.Fatal("create hook called on the existing-file guard path")
	}
}

// TestCreateBucketZfsDataset_CustomBucketGuard: a config-declared custom
// bucket keeps its existing wire behavior and never reaches the hooks.
func TestCreateBucketZfsDataset_CustomBucketGuard(t *testing.T) {
	env := setupS3TestEnv(t)
	customPath := filepath.Join(env.dataDir, "custom-path")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}
	installServerConfigView(serverConfigView{
		DataDir: env.dataDir + "/",
		Buckets: map[string]string{"custom-bkt": customPath},
	})
	t.Cleanup(func() { installServerConfigView(serverConfigView{DataDir: env.dataDir + "/"}) })

	hookCalled := false
	installTestHooks(t,
		func(_ context.Context, _ string) (string, error) { hookCalled = true; return "", nil },
		nil, nil,
	)

	w := callCreateBucket(t, "custom-bkt")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent custom bucket), got %d: %s", w.Code, w.Body.String())
	}
	if hookCalled {
		t.Fatal("create hook called on the custom-bucket guard path")
	}
}

// ---- delete path (Task 2) ----

// TestDeleteBucketZfsDataset_FeatureOffLegacyRemoveAll: hooks nil → the
// legacy RemoveAll path deletes the plain dir.
func TestDeleteBucketZfsDataset_FeatureOffLegacyRemoveAll(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "legacy-del-bkt")

	w := callDeleteBucket(t, "legacy-del-bkt")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "legacy-del-bkt")); !os.IsNotExist(err) {
		t.Fatal("legacy RemoveAll should have removed the bucket dir")
	}
}

// TestDeleteBucketZfsDataset_DestroySuccess: exists hook true, destroy
// OK → 204, the RemoveAll path is NOT taken (dir still present after
// the handler returns, since the fake destroy does not remove it).
func TestDeleteBucketZfsDataset_DestroySuccess(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-ok")
	var destroyed string

	installTestHooks(t, nil,
		func(_ context.Context, dataset string) error {
			destroyed = dataset
			return nil
		},
		func(_ context.Context, dataset string) (bool, error) {
			return dataset == "pool/data/zds-del-ok", nil
		},
	)

	w := callDeleteBucket(t, "zds-del-ok")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if destroyed != "pool/data/zds-del-ok" {
		t.Fatalf("destroy hook got dataset %q, want pool/data/zds-del-ok", destroyed)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-del-ok")); err != nil {
		t.Fatalf("RemoveAll must NOT run on a live mountpoint (dir missing): %v", err)
	}
}

// TestDeleteBucketZfsDataset_Snapshots409: destroy refuses with
// ErrDatasetHasSnapshots → 409 BucketHasSnapshots with the snapshot
// count and the `zfs destroy <ds>@<snap>` hint in the body; the dir is
// NOT removed.
func TestDeleteBucketZfsDataset_Snapshots409(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-snaps")

	installTestHooks(t, nil,
		func(_ context.Context, _ string) error {
			return fmt.Errorf("s3: destroy pool/data/zds-del-snaps refused: 2 snapshot(s): %w", ErrDatasetHasSnapshots)
		},
		func(_ context.Context, _ string) (bool, error) { return true, nil },
	)

	w := callDeleteBucket(t, "zds-del-snaps")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketHasSnapshots") {
		t.Fatalf("expected BucketHasSnapshots code, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "2") {
		t.Fatalf("expected snapshot count 2 in body, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "zfs destroy pool/data/zds-del-snaps@") {
		t.Fatalf("expected zfs destroy hint in body, got: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-del-snaps")); err != nil {
		t.Fatalf("dir must NOT be removed when destroy refuses: %v", err)
	}
}

// TestDeleteBucketZfsDataset_DestroyOtherError: any other destroy error
// → 500 fail-loud; dir NOT removed (never guess RemoveAll).
func TestDeleteBucketZfsDataset_DestroyOtherError(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-err")

	installTestHooks(t, nil,
		func(_ context.Context, _ string) error { return errors.New("zfs destroy: device busy") },
		func(_ context.Context, _ string) (bool, error) { return true, nil },
	)

	w := callDeleteBucket(t, "zds-del-err")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-del-err")); err != nil {
		t.Fatalf("dir must NOT be removed on destroy failure: %v", err)
	}
}

// TestDeleteBucketZfsDataset_ExistsFalseLegacyRemoveAll: exists hook
// false (pre-feature plain dir) → legacy RemoveAll path, dir gone.
func TestDeleteBucketZfsDataset_ExistsFalseLegacyRemoveAll(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-plain")

	installTestHooks(t, nil,
		func(_ context.Context, _ string) error {
			t.Error("destroy hook must not be called for a plain dir")
			return nil
		},
		func(_ context.Context, _ string) (bool, error) { return false, nil },
	)

	w := callDeleteBucket(t, "zds-del-plain")

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-del-plain")); !os.IsNotExist(err) {
		t.Fatal("legacy RemoveAll should have removed the plain dir")
	}
}

// TestDeleteBucketZfsDataset_ExistsHookError: exists hook errors → 500
// fail-loud (do not guess RemoveAll); dir NOT removed.
func TestDeleteBucketZfsDataset_ExistsHookError(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-exists-err")

	installTestHooks(t, nil,
		func(_ context.Context, _ string) error {
			t.Error("destroy hook must not be called when the exists probe fails")
			return nil
		},
		func(_ context.Context, _ string) (bool, error) { return false, errors.New("zfs list timed out") },
	)

	w := callDeleteBucket(t, "zds-del-exists-err")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "zds-del-exists-err")); err != nil {
		t.Fatalf("dir must NOT be removed when the exists probe fails: %v", err)
	}
}

// TestDeleteBucketZfsDataset_NotEmptyBeforeHooks: a non-empty dataset
// dir gets the existing BucketNotEmpty 409 BEFORE any hook call.
func TestDeleteBucketZfsDataset_NotEmptyBeforeHooks(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-nonempty")
	if err := os.WriteFile(filepath.Join(env.dataDir, "zds-del-nonempty", "obj"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	installTestHooks(t, nil,
		func(_ context.Context, _ string) error { hookCalled = true; return nil },
		func(_ context.Context, _ string) (bool, error) { hookCalled = true; return true, nil },
	)

	w := callDeleteBucket(t, "zds-del-nonempty")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 BucketNotEmpty, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BucketNotEmpty") {
		t.Fatalf("expected BucketNotEmpty code, got: %s", w.Body.String())
	}
	if hookCalled {
		t.Fatal("hooks called before the emptiness check — data-loss-order violation")
	}
}

// TestDeleteBucketZfsDataset_InFlightUploadsBeforeHooks: an in-flight
// multipart upload (.uploads/*.json) is also caught BEFORE the hooks.
func TestDeleteBucketZfsDataset_InFlightUploadsBeforeHooks(t *testing.T) {
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "zds-del-uploads")
	up := filepath.Join(env.dataDir, "zds-del-uploads", ".metadata", ".uploads")
	if err := os.MkdirAll(up, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(up, "upload-x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	installTestHooks(t, nil,
		func(_ context.Context, _ string) error { hookCalled = true; return nil },
		func(_ context.Context, _ string) (bool, error) { hookCalled = true; return true, nil },
	)

	w := callDeleteBucket(t, "zds-del-uploads")

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if hookCalled {
		t.Fatal("hooks called on the in-flight-uploads guard path")
	}
}

// TestDeleteBucketZfsDataset_CustomBucketGuard: a custom bucket keeps
// its 403 guard and never reaches the hooks.
func TestDeleteBucketZfsDataset_CustomBucketGuard(t *testing.T) {
	env := setupS3TestEnv(t)
	customPath := filepath.Join(env.dataDir, "custom-del-path")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}
	installServerConfigView(serverConfigView{
		DataDir: env.dataDir + "/",
		Buckets: map[string]string{"custom-del-bkt": customPath},
	})
	t.Cleanup(func() { installServerConfigView(serverConfigView{DataDir: env.dataDir + "/"}) })

	hookCalled := false
	installTestHooks(t, nil,
		func(_ context.Context, _ string) error { hookCalled = true; return nil },
		func(_ context.Context, _ string) (bool, error) { hookCalled = true; return true, nil },
	)

	w := callDeleteBucket(t, "custom-del-bkt")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for custom bucket delete, got %d: %s", w.Code, w.Body.String())
	}
	if hookCalled {
		t.Fatal("hooks called on the custom-bucket delete guard path")
	}
}
