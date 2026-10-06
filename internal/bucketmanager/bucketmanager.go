// Package bucketmanager owns bucket lifecycle (create/delete/exists/list) for
// the whole process. It was extracted from internal/frontend/s3
// (bucket_handlers.go createBucketHandler/deleteBucketHandler and
// zfsdatasets_handlers.go) so the S3 frontend and the management API call ONE
// implementation: the S3 wire behavior is preserved exactly, and the
// management path additionally refuses to destroy a ZFS dataset (user
// decision 5).
//
// The package holds no persistent state (dumb gateway charter): it reads and
// writes the backing filesystem, and the dataset feature is reached through
// the injected Provisioner.
//
// Callers:
//   - the S3 frontend installs an Env built from its live seams and calls the
//     package functions (AllowDatasetDestroy true keeps today's behavior);
//   - package main installs the production Env at startup for the management
//     API (leaf 04), which passes AllowDatasetDestroy false.
package bucketmanager

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ErrDatasetBucketNotDeletable is returned by Delete on a dataset-backed
// bucket when AllowDatasetDestroy is false: the management API cannot destroy
// ZFS datasets (user decision 5); dataset destruction stays a host-level
// operator action.
var ErrDatasetBucketNotDeletable = errors.New("bucketmanager: bucket is a ZFS dataset; destroy it on the host")

// DeleteOptions carries the caller's destructive scope.
type DeleteOptions struct {
	// AllowDatasetDestroy mirrors the S3 frontend's existing behavior. The
	// S3 handler passes true (zfs destroy, plus the existing 409
	// BucketHasSnapshots refusal); the management API passes false.
	AllowDatasetDestroy bool
}

// Locker is the per-path write serialization surface (the ONE process lock
// table, internal/fslock). Lock takes the per-path lock and returns its
// release function.
type Locker interface {
	Lock(path string) func()
}

// Provisioner is the ZFS bucket-dataset seam, nil when the dataset feature is
// off (legacy plain-directory path).
type Provisioner interface {
	// Create provisions the bucket as its own dataset and returns the
	// dataset name.
	Create(ctx context.Context, bucket string) (dataset string, err error)
	// Destroy destroys the dataset; a snapshot-bearing dataset is refused by
	// the implementation (never -r).
	Destroy(ctx context.Context, dataset string) error
	// Exists probes whether the bucket's dataset exists by name.
	Exists(ctx context.Context, dataset string) (bool, error)
	// Parent returns the configured parent dataset prefix ("" = off).
	Parent() string
}

// Env is the installed environment: the process lock table, the bucket path
// resolver, the custom-bucket lookup, and the optional dataset provisioner.
type Env struct {
	Locks       Locker
	BucketPath  func(bucket string) string
	Custom      func(bucket string) (path string, ok bool)
	Provisioner Provisioner
}

// BucketInfo is one listed bucket: its name and creation (directory) time.
type BucketInfo struct {
	Name      string
	CreatedAt time.Time
}

// DatasetDestroyError wraps a Provisioner.Destroy failure with the dataset
// name, so the S3 frontend can reproduce the exact 409 BucketHasSnapshots
// message (which needs the dataset for the `zfs destroy <ds>@<snap>` hint)
// without the shared package importing the s3 sentinel.
type DatasetDestroyError struct {
	Dataset string
	Err     error
}

func (e *DatasetDestroyError) Error() string { return e.Err.Error() }
func (e *DatasetDestroyError) Unwrap() error { return e.Err }

var (
	envMu        sync.RWMutex
	installedEnv *Env
)

// Install sets the process bucket-manager environment. The wiring layer calls
// it at startup; the S3 frontend refreshes it from its live seams per request
// (mirroring the config-view re-mirror pattern), and tests re-install per
// case.
func Install(env Env) {
	envMu.Lock()
	defer envMu.Unlock()
	e := env
	installedEnv = &e
}

// currentEnv returns the installed environment (zero value when none).
func currentEnv() Env {
	envMu.RLock()
	defer envMu.RUnlock()
	if installedEnv == nil {
		return Env{}
	}
	return *installedEnv
}

// errNotInstalled reports a missing environment as an InternalError (never a
// nil dereference in a library path).
func errNotInstalled() error {
	return objectmodel.NewError(objectmodel.CodeInternalError, "Internal Server Error", 500)
}

// Create creates bucket name: the bucket-name rule, the custom-bucket guard,
// the existing-path guards, then either the dataset provisioning path or the
// plain-directory MkdirAll path. Behavior is byte-identical to the pre-move S3
// handler for every name the S3 frontend admits (it validates first, so the
// second check is unreachable there and harmless).
func Create(ctx context.Context, name string) error {
	return currentEnv().create(ctx, name)
}

// Delete deletes bucket name: the bucket-name rule, the custom-bucket 403
// guard, then the exist / emptiness / in-flight-upload guards, then the
// dataset or plain-directory branch. opts.AllowDatasetDestroy gates dataset
// destruction.
func Delete(ctx context.Context, name string, opts DeleteOptions) error {
	return currentEnv().delete(ctx, name, opts)
}

// Exists reports whether bucket name exists as a directory.
func Exists(ctx context.Context, name string) (bool, error) {
	return currentEnv().exists(ctx, name)
}

// List lists the buckets discovered under the configured data root (the
// auto-provisioned, non-hidden directories), sorted by name.
func List(ctx context.Context) ([]BucketInfo, error) {
	return currentEnv().list(ctx)
}

func (e Env) create(ctx context.Context, name string) error {
	if e.BucketPath == nil || e.Locks == nil {
		return errNotInstalled()
	}
	// Bucket-name rule FIRST: an invalid name must be refused before any path
	// is resolved or any filesystem/provisioner work happens (bughunt H3).
	if err := e.validateName(name); err != nil {
		return err
	}
	// Custom-configured buckets are config-controlled: if the configured path
	// exists on disk, PUT is idempotent success; if missing, report 409 with
	// a message about the custom path (still no create).
	if customPath, isCustom := e.Custom(name); isCustom {
		return e.createCustom(name, customPath)
	}

	bucketPath := e.BucketPath(name)
	metadataPath := filepath.Join(bucketPath, ".metadata")

	// Serialize create against delete of the same bucket name (a concurrent
	// delete could remove the directory between Mkdir(bucket) and
	// Mkdir(.metadata), turning the create into a spurious 500).
	unlock := e.Locks.Lock(bucketPath)
	defer unlock()

	if err := e.ensureCreatable(bucketPath, name); err != nil {
		return err
	}

	if e.Provisioner != nil {
		return e.createDataset(ctx, name, bucketPath, metadataPath)
	}
	return e.createPlain(name, bucketPath, metadataPath)
}

// createCustom handles a config-declared custom bucket (create is idempotent
// when its path exists, 409 when missing; never created via the API).
func (e Env) createCustom(name, customPath string) error {
	info, err := os.Stat(customPath)
	if err != nil || !info.IsDir() {
		log.Printf("Bucket %s is a custom-configured bucket whose path %s is missing on disk.", strconv.Quote(name), customPath)
		return objectmodel.NewError("BucketAlreadyExists",
			"The requested bucket name is a custom-configured bucket whose path is missing on disk; it cannot be created via the API.",
			409)
	}
	log.Printf("Bucket %s is a custom-configured bucket, already exists.", strconv.Quote(name))
	return nil // Idempotent
}

// ensureCreatable applies the pre-move stat guards: a stat error that is
// neither nil nor IsNotExist is a 500; an existing file is 409
// BucketAlreadyExists; an existing directory is 409 BucketAlreadyOwnedByYou.
//
// Reached only through Env.create, which has ALREADY run Env.validateName on
// name, so bucketPath is one path segment under the resolved data root. (This
// comment previously claimed a validateBucketName check that no caller of this
// function ever performed — see bughunt H3.)
func (e Env) ensureCreatable(bucketPath, name string) error {
	info, err := os.Stat(bucketPath) //nolint:gosec // G703: G703 is excluded repo-wide (.golangci.yml); name validated by Env.validateName in Env.create
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Error statting bucket path %s: %v", bucketPath, err) //nolint:gosec // G703
		return objectmodel.NewError(objectmodel.CodeInternalError, "Internal Server Error", 500)
	}
	if err == nil {
		if !info.IsDir() {
			log.Printf("Path %s exists as a file; cannot create bucket %s.", bucketPath, name) //nolint:gosec // G703
			return objectmodel.NewError("BucketAlreadyExists", "The requested bucket name is not available.", 409)
		}
		log.Printf("Bucket %s already exists.", strconv.Quote(name))
		return objectmodel.NewError("BucketAlreadyOwnedByYou",
			"Your previous request to create the named bucket succeeded and you already own it.", 409)
	}
	return nil
}

// createDataset provisions the bucket as its own ZFS dataset; on a .metadata
// mkdir failure it rolls the dataset back through Destroy (a refused rollback
// because snapshots raced the create still answers 500 and leaves the empty
// dataset — never -r).
func (e Env) createDataset(ctx context.Context, name, bucketPath, metadataPath string) error {
	dataset, createErr := e.Provisioner.Create(ctx, name)
	if createErr != nil {
		log.Printf("Error creating ZFS dataset for bucket %s: %v", strconv.Quote(name), createErr)
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error creating bucket.", 500)
	}

	//nolint:gosec // G703: G703 is excluded repo-wide (.golangci.yml); metadataPath is under the bucketPath validated in Env.create
	if err := os.Mkdir(metadataPath, 0755); err != nil {
		log.Printf("Error creating metadata directory %s for bucket %s: %v — rolling back dataset %s", metadataPath, name, err, dataset)
		if rollbackErr := e.Provisioner.Destroy(ctx, dataset); rollbackErr != nil {
			log.Printf("CRITICAL: rollback destroy of dataset %s failed after metadata mkdir failure for bucket %s: %v — the empty dataset is left in place; remove it manually (zfs destroy %s), snapshots are never auto-destroyed", dataset, strconv.Quote(name), rollbackErr, dataset)
		}
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error creating bucket metadata storage.", 500)
	}

	log.Printf("Successfully created bucket: %s (dataset %s)", strconv.Quote(name), dataset)
	return nil
}

// createPlain is the legacy plain-directory path.
func (e Env) createPlain(name, bucketPath, metadataPath string) error {
	if err := os.MkdirAll(bucketPath, 0755); err != nil { //nolint:gosec // G703
		log.Printf("Error creating bucket directory %s: %v", bucketPath, err)
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error creating bucket.", 500)
	}
	if err := os.Mkdir(metadataPath, 0755); err != nil { //nolint:gosec // G703
		log.Printf("Error creating metadata directory %s for bucket %s: %v", metadataPath, name, err) //nolint:gosec // G706
		os.RemoveAll(bucketPath)                                                                      //nolint:gosec // G703
		return objectmodel.NewError(objectmodel.CodeInternalError, "Error creating bucket metadata storage.", 500)
	}
	log.Printf("Successfully created bucket: %s", strconv.Quote(name))
	return nil
}

func (e Env) exists(_ context.Context, name string) (bool, error) {
	if e.BucketPath == nil {
		return false, errNotInstalled()
	}
	info, err := os.Stat(e.BucketPath(name)) //nolint:gosec // G703
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

func (e Env) list(_ context.Context) ([]BucketInfo, error) {
	if e.BucketPath == nil {
		return nil, errNotInstalled()
	}
	root := e.BucketPath("")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]BucketInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		out = append(out, BucketInfo{Name: entry.Name(), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
