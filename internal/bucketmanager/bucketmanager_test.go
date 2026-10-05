package bucketmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/fslock"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// fakeProv records calls and returns scripted results; nil funcs fall back to
// the plain-dir-ish defaults.
type fakeProv struct {
	parent       string
	createFn     func(ctx context.Context, bucket string) (string, error)
	destroyFn    func(ctx context.Context, dataset string) error
	existsFn     func(ctx context.Context, dataset string) (bool, error)
	createCalls  []string
	destroyCalls []string
	existsCalls  []string
}

func (p *fakeProv) Create(ctx context.Context, bucket string) (string, error) {
	p.createCalls = append(p.createCalls, bucket)
	if p.createFn != nil {
		return p.createFn(ctx, bucket)
	}
	return p.parent + "/" + bucket, nil
}

func (p *fakeProv) Destroy(ctx context.Context, dataset string) error {
	p.destroyCalls = append(p.destroyCalls, dataset)
	if p.destroyFn != nil {
		return p.destroyFn(ctx, dataset)
	}
	return nil
}

func (p *fakeProv) Exists(ctx context.Context, dataset string) (bool, error) {
	p.existsCalls = append(p.existsCalls, dataset)
	if p.existsFn != nil {
		return p.existsFn(ctx, dataset)
	}
	return false, nil
}

func (p *fakeProv) Parent() string { return p.parent }

// setupEnv installs a fresh test environment rooted at a temp dir. It returns
// the root, the mutable env (so tests can add a Provisioner/Custom map), and
// the fake provisioner.
func setupEnv(t *testing.T) (string, *Env, *fakeProv) {
	t.Helper()
	root := t.TempDir()
	prov := &fakeProv{parent: "pool/data"}
	env := &Env{
		Locks:       fslock.Default,
		BucketPath:  func(bucket string) string { return filepath.Join(root, bucket) },
		Custom:      func(string) (string, bool) { return "", false },
		Provisioner: nil,
	}
	t.Cleanup(func() { Install(Env{}) })
	return root, env, prov
}

func errCode(err error) string {
	if oe, ok := errors.AsType[*objectmodel.Error](err); ok {
		return oe.Code
	}
	return ""
}

func makeBucket(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(path, ".metadata"), 0o755); err != nil {
		t.Fatalf("setup bucket: %v", err)
	}
	return path
}

func TestCreateFreshPlainBucket(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)

	if err := Create(context.Background(), "fresh-bucket"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range []string{filepath.Join(root, "fresh-bucket"), filepath.Join(root, "fresh-bucket", ".metadata")} {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			t.Fatalf("%s missing after create: %v", p, err)
		}
	}
}

func TestCreateExistingDirIsOwnedConflict(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	makeBucket(t, root, "exists")

	err := Create(context.Background(), "exists")
	if got := errCode(err); got != "BucketAlreadyOwnedByYou" {
		t.Fatalf("err = %v (code %q), want BucketAlreadyOwnedByYou", err, got)
	}
}

func TestCreatePathIsFileConflict(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	if err := os.WriteFile(filepath.Join(root, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Create(context.Background(), "occupied")
	if got := errCode(err); got != "BucketAlreadyExists" {
		t.Fatalf("err = %v (code %q), want BucketAlreadyExists", err, got)
	}
}

func TestCreateCustomBucketGuards(t *testing.T) {
	root, env, prov := setupEnv(t)
	customPath := filepath.Join(root, "custom-area")
	env.Custom = func(bucket string) (string, bool) {
		if bucket == "custom-bkt" {
			return customPath, true
		}
		return "", false
	}
	env.Provisioner = prov
	Install(*env)

	// Missing custom path → 409 BucketAlreadyExists, provisioner untouched.
	if err := Create(context.Background(), "custom-bkt"); errCode(err) != "BucketAlreadyExists" {
		t.Fatalf("missing custom path: err = %v (code %q)", err, errCode(err))
	}
	if len(prov.createCalls) != 0 {
		t.Fatalf("provisioner called for custom bucket: %v", prov.createCalls)
	}

	// Existing custom path → idempotent success, provisioner untouched.
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), "custom-bkt"); err != nil {
		t.Fatalf("existing custom path: %v", err)
	}
	if len(prov.createCalls) != 0 {
		t.Fatalf("provisioner called for custom bucket: %v", prov.createCalls)
	}
}

func TestDeleteNonEmptyRefusedBeforeProvisioner(t *testing.T) {
	root, env, prov := setupEnv(t)
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "nonempty")
	if err := os.WriteFile(filepath.Join(path, "obj"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Delete(context.Background(), "nonempty", DeleteOptions{AllowDatasetDestroy: true})
	if errCode(err) != "BucketNotEmpty" {
		t.Fatalf("err = %v (code %q), want BucketNotEmpty", err, errCode(err))
	}
	if len(prov.existsCalls)+len(prov.destroyCalls) != 0 {
		t.Fatalf("provisioner touched before emptiness: exists=%v destroy=%v", prov.existsCalls, prov.destroyCalls)
	}
}

func TestDeleteInFlightUploadsRefusedBeforeProvisioner(t *testing.T) {
	root, env, prov := setupEnv(t)
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "inflight")
	uploads := filepath.Join(path, ".metadata", ".uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "u.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Delete(context.Background(), "inflight", DeleteOptions{AllowDatasetDestroy: true})
	if errCode(err) != "BucketNotEmpty" {
		t.Fatalf("err = %v (code %q), want BucketNotEmpty", err, errCode(err))
	}
	if len(prov.existsCalls)+len(prov.destroyCalls) != 0 {
		t.Fatalf("provisioner touched before uploads check: %v", prov)
	}
}

// TestDeleteIgnoresControlDirs is the regression guard for the live bug fixed
// in 213f485: .metadata, .bucket-actions and .zfs never block DeleteBucket.
func TestDeleteIgnoresControlDirs(t *testing.T) {
	root, env, prov := setupEnv(t)
	prov.existsFn = func(context.Context, string) (bool, error) { return false, nil }
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "control-dirs")
	for _, d := range []string{".bucket-actions", ".zfs"} {
		if err := os.MkdirAll(filepath.Join(path, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := Delete(context.Background(), "control-dirs", DeleteOptions{AllowDatasetDestroy: true}); err != nil {
		t.Fatalf("Delete with control dirs: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bucket dir should be removed, stat err = %v", err)
	}
	if len(prov.destroyCalls) != 0 {
		t.Fatalf("destroy called for a plain dir: %v", prov.destroyCalls)
	}
}

func TestDeleteCustomBucketForbidden(t *testing.T) {
	root, env, prov := setupEnv(t)
	customPath := filepath.Join(root, "custom-del")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}
	env.Custom = func(bucket string) (string, bool) {
		if bucket == "custom-del" {
			return customPath, true
		}
		return "", false
	}
	env.Provisioner = prov
	Install(*env)

	err := Delete(context.Background(), "custom-del", DeleteOptions{AllowDatasetDestroy: true})
	if errCode(err) != "AccessDenied" {
		t.Fatalf("err = %v (code %q), want AccessDenied", err, errCode(err))
	}
	if len(prov.existsCalls)+len(prov.destroyCalls) != 0 {
		t.Fatalf("provisioner touched for custom bucket: %v", prov)
	}
}

func TestDeleteDatasetRefusedWhenNotAllowed(t *testing.T) {
	root, env, prov := setupEnv(t)
	prov.existsFn = func(_ context.Context, dataset string) (bool, error) {
		return dataset == "pool/data/data-bkt", nil
	}
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "data-bkt")

	err := Delete(context.Background(), "data-bkt", DeleteOptions{AllowDatasetDestroy: false})
	if !errors.Is(err, ErrDatasetBucketNotDeletable) {
		t.Fatalf("err = %v, want ErrDatasetBucketNotDeletable", err)
	}
	if len(prov.destroyCalls) != 0 {
		t.Fatalf("Destroy called on the refused management path: %v", prov.destroyCalls)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("refused delete removed the bucket dir: %v", statErr)
	}
}

func TestDeleteDatasetAllowedDestroys(t *testing.T) {
	root, env, prov := setupEnv(t)
	prov.existsFn = func(_ context.Context, dataset string) (bool, error) {
		return dataset == "pool/data/data-bkt", nil
	}
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "data-bkt")

	if err := Delete(context.Background(), "data-bkt", DeleteOptions{AllowDatasetDestroy: true}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(prov.destroyCalls) != 1 || prov.destroyCalls[0] != "pool/data/data-bkt" {
		t.Fatalf("destroy calls = %v, want [pool/data/data-bkt]", prov.destroyCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("RemoveAll must NOT run on a live mountpoint: %v", err)
	}
}

func TestDeletePlainBucketRemoves(t *testing.T) {
	root, env, prov := setupEnv(t)
	env.Provisioner = prov
	Install(*env)
	path := makeBucket(t, root, "plain-del")

	if err := Delete(context.Background(), "plain-del", DeleteOptions{AllowDatasetDestroy: true}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plain bucket not removed: %v", err)
	}
	if len(prov.destroyCalls) != 0 {
		t.Fatalf("destroy called for plain dir: %v", prov.destroyCalls)
	}
}

func TestDeleteNonExistent(t *testing.T) {
	_, env, _ := setupEnv(t)
	Install(*env)
	if err := Delete(context.Background(), "ghost", DeleteOptions{AllowDatasetDestroy: true}); errCode(err) != "NoSuchBucket" {
		t.Fatalf("err = %v (code %q), want NoSuchBucket", err, errCode(err))
	}
}

func TestCreateProvisionerFailureNoFallback(t *testing.T) {
	root, env, prov := setupEnv(t)
	prov.createFn = func(_ context.Context, _ string) (string, error) {
		return "", errors.New("pool is full")
	}
	env.Provisioner = prov
	Install(*env)

	err := Create(context.Background(), "ds-fail")
	if errCode(err) != "InternalError" {
		t.Fatalf("err = %v (code %q), want InternalError", err, errCode(err))
	}
	if _, statErr := os.Stat(filepath.Join(root, "ds-fail")); !os.IsNotExist(statErr) {
		t.Fatalf("mkdir fallback ran after provisioner failure")
	}
}

func TestCreateDatasetMetadataRollback(t *testing.T) {
	root, env, prov := setupEnv(t)
	// provisioner claims success but does not create the mountpoint, so the
	// .metadata mkdir fails and triggers the rollback Destroy.
	env.Provisioner = prov
	Install(*env)

	err := Create(context.Background(), "ds-rollback")
	if errCode(err) != "InternalError" {
		t.Fatalf("err = %v (code %q), want InternalError", err, errCode(err))
	}
	if len(prov.destroyCalls) != 1 || prov.destroyCalls[0] != "pool/data/ds-rollback" {
		t.Fatalf("rollback destroy calls = %v, want [pool/data/ds-rollback]", prov.destroyCalls)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ds-rollback")); !os.IsNotExist(statErr) {
		t.Fatalf("rollback should leave no bucket dir behind")
	}
}

func TestExistsAndList(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	makeBucket(t, root, "b-bucket")
	makeBucket(t, root, "a-bucket")
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	ok, err := Exists(context.Background(), "a-bucket")
	if err != nil || !ok {
		t.Fatalf("Exists(a-bucket) = %v, %v", ok, err)
	}
	ok, err = Exists(context.Background(), "ghost")
	if err != nil || ok {
		t.Fatalf("Exists(ghost) = %v, %v", ok, err)
	}

	buckets, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make([]string, 0, len(buckets))
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	if len(names) != 2 || names[0] != "a-bucket" || names[1] != "b-bucket" {
		t.Fatalf("List = %v, want [a-bucket b-bucket]", names)
	}
}

// TestConcurrentCreateSharesOneLockTable proves a caller holding the process
// lock for a bucket path blocks a concurrent Create of the same bucket — i.e.
// the manager and any other caller share ONE lock table.
func TestConcurrentCreateSharesOneLockTable(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	bucketPath := filepath.Join(root, "locked-bucket")

	release := fslock.Default.Lock(bucketPath)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Create(context.Background(), "locked-bucket"); err != nil {
			t.Errorf("Create: %v", err)
		}
	}()

	select {
	case <-done:
		t.Fatal("Create did not block on the held per-path lock")
	case <-time.After(150 * time.Millisecond):
	}

	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Create did not proceed after the lock was released")
	}
}
