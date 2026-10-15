package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// A missing live file must not resolve through the event log. A parent
// rename leaves the old full_path in events; tagging that key would
// write tags onto the object that now lives elsewhere.
func TestTagObjectIDMissingFileDoesNotUseEventLog(t *testing.T) {
	dir := t.TempDir()
	called := false
	store := &zmetadTagStore{
		bucketPath:   dir,
		statObjectID: objectIDFromStat,
		resolveObjectID: func(context.Context, string, string) (uint64, error) {
			called = true
			return 99, nil
		},
	}
	_, err := store.objectID(context.Background(), "gone.txt")
	var oe *objectmodel.Error
	if !errors.As(err, &oe) || oe.Code != objectmodel.CodeNoSuchKey {
		t.Fatalf("missing file err = %v, want NoSuchKey", err)
	}
	if called {
		t.Fatal("event resolver ran for a missing file")
	}
}

// An existing file on Linux uses st_ino and does not call the event
// resolver. On other OS the stat path reports unsupported and the
// injected resolver may run; this test pins the Linux half only.
func TestTagObjectIDLiveFileSkipsEventLogOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("st_ino is the ZFS object id only on Linux")
	}
	dir := t.TempDir()
	path := objectDataPathFor(dir, "live.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	store := &zmetadTagStore{
		bucketPath:   dir,
		statObjectID: objectIDFromStat,
		resolveObjectID: func(context.Context, string, string) (uint64, error) {
			called = true
			return 99, nil
		},
	}
	id, err := store.objectID(context.Background(), "live.txt")
	if err != nil {
		t.Fatalf("objectID: %v", err)
	}
	if id == 0 || id == 99 {
		t.Fatalf("id = %d, want the file inode, not the event-log stub", id)
	}
	if called {
		t.Fatal("event resolver ran for a live file")
	}
}

// A tag-store failure after CopyObject must not drop the recorded
// version. The Put already rewrote the sidecar.
func TestCopyObjectTagFailureStillRecordsVersion(t *testing.T) {
	env := newVerEnv(t, "tagfail-copy-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if w := verPutBody(t, env, "dst.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put dst: %d %s", w.Code, w.Body.String())
	}
	srcReq := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/src.txt", strings.NewReader("copied"))
	srcReq.Header.Set("x-amz-tagging", "env=prod")
	srcW := httptest.NewRecorder()
	putObjectHandler(srcW, srcReq, env.bucket, "src.txt")
	if srcW.Code != http.StatusOK {
		t.Fatalf("put src: %d %s", srcW.Code, srcW.Body.String())
	}

	real := tagStoreFor(env.bucketPath)
	tagStoreForTest = func(string) tagStore { return failPutTagStore{inner: real} }
	t.Cleanup(func() { tagStoreForTest = nil })

	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/dst.txt", nil)
	req.Header.Set("x-amz-copy-source", "/"+env.bucket+"/src.txt")
	w := httptest.NewRecorder()
	copyObjectHandler(w, req, env.bucket, "dst.txt")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("copy = %d %s, want 500 from the tag store", w.Code, w.Body.String())
	}
	versions, err := versionStoreForBucket(env.bucketPath).List(env.bucket, "dst.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions after tag failure = %d, want 1 (the pre-copy bytes recorded)", len(versions))
	}
}

type failPutTagStore struct{ inner tagStore }

func (s failPutTagStore) Get(key string) (map[string]string, error) { return s.inner.Get(key) }
func (s failPutTagStore) Put(string, map[string]string) error {
	return objectmodel.ErrInternalError("tag store down")
}
func (s failPutTagStore) Delete(key string) error { return s.inner.Delete(key) }
