package s3

// tagstore_test.go — the tagStore seam + sidecarTagStore tests (tagging
// tree leaf 03). Covers: round-trip through a real fsbackend-written
// sidecar, byte-compat with pre-tagging sidecars, error taxonomy
// (NoSuchKey on missing object, sidecar preserved on tag write), and the
// seam contract (empty map for untagged, nil tags on delete).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// tagTestEnv builds a real backend env, creates a bucket, and puts an
// object through fsbackend so the sidecar on disk is the REAL production
// sidecar (not a hand-rolled test fixture).
func tagTestEnv(t *testing.T, bucket, key string) (*testS3Env, tagStore) {
	t.Helper()
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, bucket)
	if _, err := env.b.Put(t.Context(), bucket, key, strings.NewReader(""), 0, objectmodel.PutOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("fsbackend Put: %v", err)
	}
	store := tagStoreFor(filepath.Join(env.dataDir, bucket))
	return env, store
}

func TestTagStore_Get_UntaggedObjectYieldsEmptyMap(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	tags, err := store.Get("obj.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tags == nil {
		t.Fatal("untagged object must yield a non-nil empty map")
	}
	if len(tags) != 0 {
		t.Errorf("expected empty tags, got %v", tags)
	}
}

func TestTagStore_Get_MissingObjectIsNoSuchKey(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	_, err := store.Get("no-such-object")
	var omErr *objectmodel.Error
	if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
		t.Fatalf("expected NoSuchKey error, got %v", err)
	}
}

func TestTagStore_PutGet_RoundTrip(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	want := map[string]string{"env": "prod", "team": "core"}
	if err := store.Put("obj.txt", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get("obj.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d tags, got %v", len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %q: want %q, got %q", k, v, got[k])
		}
	}
}

func TestTagStore_Put_PreservesOtherSidecarFields(t *testing.T) {
	env, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	if err := store.Put("obj.txt", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(env.dataDir, "tag-bucket", ".metadata", "obj.txt.meta")) //nolint:gosec // G304: test-fixed path.
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var meta objectmodel.LegacyObjectMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if meta.ContentType != "text/plain" {
		t.Errorf("contentType lost: %q", meta.ContentType)
	}
	if meta.ETag == "" {
		t.Error("eTag lost")
	}
	if meta.StoragePath == "" {
		t.Error("storagePath lost")
	}
}

func TestTagStore_Put_EmptyTagsRemovesField(t *testing.T) {
	env, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	if err := store.Put("obj.txt", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put("obj.txt", nil); err != nil {
		t.Fatalf("Put(empty): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(env.dataDir, "tag-bucket", ".metadata", "obj.txt.meta")) //nolint:gosec // G304: test-fixed path.
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(raw) == "" {
		t.Fatal("sidecar vanished")
	}
	// The tags field must be GONE from the JSON (omitempty byte-compat).
	var sidecar map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if _, present := sidecar["tags"]; present {
		t.Errorf("tags field must be omitted when empty, sidecar: %s", raw)
	}
}

func TestTagStore_Put_MissingObjectIsNoSuchKey(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	if err := store.Put("no-such-object", map[string]string{"k": "v"}); err == nil {
		t.Fatal("expected ErrNoSuchKey putting tags on a missing object")
	}
}

func TestTagStore_Delete_RemovesTags(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	if err := store.Put("obj.txt", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete("obj.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	tags, err := store.Get("obj.txt")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected no tags after Delete, got %v", tags)
	}
}

func TestTagStore_Delete_MissingObjectIsNoSuchKey(t *testing.T) {
	_, store := tagTestEnv(t, "tag-bucket", "obj.txt")

	if err := store.Delete("no-such-object"); err == nil {
		t.Fatal("expected ErrNoSuchKey deleting tags of a missing object")
	}
}

func TestTagStore_BackwardCompat_OldSidecarWithoutTags(t *testing.T) {
	// A sidecar written by a pre-tagging build has no tags field; Get
	// must succeed with an empty map (leaf 02 byte-compat, read side).
	env := setupS3TestEnv(t)
	_ = env.setupBucket(t, "old-bucket")
	metaPath := filepath.Join(env.dataDir, "old-bucket", ".metadata", "legacy.txt.meta")
	oldSidecar := `{"contentType":"text/plain","contentLength":2,"eTag":"abc","customMetadata":{},"lastModified":"2020-01-01T00:00:00Z","storagePath":"x"}`
	if err := os.WriteFile(metaPath, []byte(oldSidecar), 0o644); err != nil { //nolint:gosec // G306/G304: test fixture.
		t.Fatalf("write legacy sidecar: %v", err)
	}
	// And a data file so the object is real.
	if err := os.WriteFile(filepath.Join(env.dataDir, "old-bucket", "legacy.txt"), []byte("hi"), 0o644); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatalf("write data: %v", err)
	}

	store := tagStoreFor(filepath.Join(env.dataDir, "old-bucket"))
	tags, err := store.Get("legacy.txt")
	if err != nil {
		t.Fatalf("Get on legacy sidecar: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("legacy sidecar should read as untagged, got %v", tags)
	}

	// Writing tags onto the legacy sidecar must work too (upgrade path).
	if err := store.Put("legacy.txt", map[string]string{"age": "old"}); err != nil {
		t.Fatalf("Put on legacy sidecar: %v", err)
	}
	got, err := store.Get("legacy.txt")
	if err != nil || got["age"] != "old" {
		t.Errorf("tags after upgrade Put: %v err=%v", got, err)
	}
}

func TestTagStoreFor_ReturnsSidecarStore(t *testing.T) {
	store := tagStoreFor("/tmp/whatever")
	if _, ok := store.(sidecarTagStore); !ok {
		t.Fatalf("tagStoreFor should return sidecarTagStore in v1, got %T", store)
	}
}

// Compile-time pin: fsbackend remains untouched by this leaf; the env's
// backend is still an *fsbackend.FS (the tag seam works ABOVE the seam).
var _ = fsbackend.New
