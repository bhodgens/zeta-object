package s3

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	"github.com/bhodgens/zeta-object/internal/fslock"
)

// saveRestoreDatasetHooks saves the dataset hook vars + parent and restores
// them on cleanup.
func saveRestoreDatasetHooks(t *testing.T) {
	t.Helper()
	origCreate, origDestroy, origExists := zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists
	origParent := zfsBucketDatasetParent
	t.Cleanup(func() {
		zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists = origCreate, origDestroy, origExists
		zfsBucketDatasetParent = origParent
	})
}

func TestNewDatasetProvisionerNilWhenOff(t *testing.T) {
	saveRestoreDatasetHooks(t)
	zfsBucketCreate, zfsBucketDestroy, zfsBucketDatasetExists = nil, nil, nil
	zfsBucketDatasetParent = ""
	if p := NewDatasetProvisioner(); p != nil {
		t.Fatalf("NewDatasetProvisioner = %v, want nil when off", p)
	}
}

func TestDatasetProvisionerDrivesHooks(t *testing.T) {
	saveRestoreDatasetHooks(t)
	var createdBucket, destroyedDataset, probedDataset string
	zfsBucketCreate = func(_ context.Context, bucket string) (string, error) {
		createdBucket = bucket
		return "pool/data/" + bucket, nil
	}
	zfsBucketDestroy = func(_ context.Context, dataset string) error {
		destroyedDataset = dataset
		return nil
	}
	zfsBucketDatasetExists = func(_ context.Context, dataset string) (bool, error) {
		probedDataset = dataset
		return true, nil
	}
	zfsBucketDatasetParent = "pool/data"

	p := NewDatasetProvisioner()
	if p == nil {
		t.Fatal("NewDatasetProvisioner = nil with hooks installed")
	}
	if got, err := p.Create(context.Background(), "bkt"); err != nil || got != "pool/data/bkt" {
		t.Fatalf("Create = %q, %v", got, err)
	}
	if err := p.Destroy(context.Background(), "pool/data/bkt"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if ok, err := p.Exists(context.Background(), "pool/data/bkt"); err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	if p.Parent() != "pool/data" {
		t.Fatalf("Parent = %q", p.Parent())
	}
	if createdBucket != "bkt" || destroyedDataset != "pool/data/bkt" || probedDataset != "pool/data/bkt" {
		t.Fatalf("hooks not driven: created=%q destroyed=%q probed=%q", createdBucket, destroyedDataset, probedDataset)
	}
}

// recordingProvisioner is the delegation marker: the handler installs an env
// carrying it and the manager calls it.
type recordingProvisioner struct {
	createCalls []string
	parent      string
	mkdir       func(path string) error
}

func (p *recordingProvisioner) Create(_ context.Context, bucket string) (string, error) {
	p.createCalls = append(p.createCalls, bucket)
	if p.mkdir != nil {
		if err := p.mkdir(bucket); err != nil {
			return "", err
		}
	}
	return p.parent + "/" + bucket, nil
}
func (p *recordingProvisioner) Destroy(context.Context, string) error { return nil }
func (p *recordingProvisioner) Exists(context.Context, string) (bool, error) {
	return false, nil
}
func (p *recordingProvisioner) Parent() string { return p.parent }

// TestHandlerDelegatesToBucketManager proves createBucketHandler routes through
// the installed bucket-manager environment: the override env's provisioner
// records the call (an inline handler would never touch it).
func TestHandlerDelegatesToBucketManager(t *testing.T) {
	root := t.TempDir()
	prov := &recordingProvisioner{
		parent: "pool/data",
		mkdir:  func(bucket string) error { return os.MkdirAll(filepath.Join(root, bucket), 0o755) },
	}
	orig := bucketManagerEnvOverride
	bucketManagerEnvOverride = func() bucketmanager.Env {
		return bucketmanager.Env{
			Locks:       fslock.Default,
			BucketPath:  func(bucket string) string { return filepath.Join(root, bucket) },
			Custom:      func(string) (string, bool) { return "", false },
			Provisioner: prov,
		}
	}
	t.Cleanup(func() {
		bucketManagerEnvOverride = orig
		bucketmanager.Install(bucketmanager.Env{})
	})

	w := callCreateBucket(t, "delegated-bkt")
	if w.Code != 200 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if len(prov.createCalls) != 1 || prov.createCalls[0] != "delegated-bkt" {
		t.Fatalf("provisioner create calls = %v, want [delegated-bkt] (handler did not delegate)", prov.createCalls)
	}
}

// TestManagerAndS3PathShareLockTable proves an S3-path lock and a manager call
// on the same bucket contending on ONE lock table: a manager Create blocks
// while the S3 lockObject for that bucket path is held.
func TestManagerAndS3PathShareLockTable(t *testing.T) {
	root := t.TempDir()
	bucketmanager.Install(bucketmanager.Env{
		Locks:       fslock.Default,
		BucketPath:  func(bucket string) string { return filepath.Join(root, bucket) },
		Custom:      func(string) (string, bool) { return "", false },
		Provisioner: nil,
	})
	t.Cleanup(func() { bucketmanager.Install(bucketmanager.Env{}) })

	bucketPath := filepath.Join(root, "shared-lock-bkt")
	unlock := lockObject(bucketPath) // the S3-path lock (delegates to fslock)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := bucketmanager.Create(context.Background(), "shared-lock-bkt"); err != nil {
			t.Errorf("manager Create: %v", err)
		}
	}()

	select {
	case <-done:
		t.Fatal("manager Create did not block on the S3-path lock (two lock tables?)")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("manager Create did not proceed after the S3-path lock released")
	}

	if _, err := os.Stat(filepath.Join(bucketPath, ".metadata")); err != nil {
		t.Fatalf(".metadata not created after lock released: %v", err)
	}
}
