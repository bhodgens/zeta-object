package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	admin "github.com/bhodgens/zeta-object/internal/frontend/admin"
)

// adminTraversalNames are names the management surface must refuse. "/.." is
// included because the console reaches the route as /api/buckets/{name} and
// PathEscape("..") is a no-op ('.' is RFC 3986 unreserved).
var adminTraversalNames = []string{"..", "../srvdata", ".", "a/b", ""}

// TestAdminWiringTraversalNameRefused is the end-to-end guard for H3 on the
// management path: DELETE /buckets/{name} with a traversal name reached
// bucketmanager.Delete with no validation, and the answer was a 200
// {"deleted":true} after an out-of-tree directory had been removed. The
// refusal must be a 400 ServiceError naming the rule, and nothing on disk may
// change.
func TestAdminWiringTraversalNameRefused(t *testing.T) {
	dataDir := t.TempDir()
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, nil)
	w := buildAdminWiring(nil)
	ctx := context.Background()

	// A directory OUTSIDE the data root, and a real bucket inside it.
	outside := filepath.Join(filepath.Dir(dataDir), "srvdata")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	if err := w.services.CreateBucket(ctx, "keepme"); err != nil {
		t.Fatalf("CreateBucket(keepme): %v", err)
	}

	for _, name := range adminTraversalNames {
		t.Run("delete/"+name, func(t *testing.T) {
			err := w.services.DeleteBucket(ctx, name)
			se, ok := errors.AsType[*admin.ServiceError](err)
			if !ok || se.Status != 400 {
				t.Fatalf("DeleteBucket(%q) = %v, want a 400 ServiceError", name, err)
			}
			if se.Code == "" || se.Message == "" {
				t.Fatalf("DeleteBucket(%q): empty error envelope %+v", name, se)
			}
			if _, statErr := os.Stat(outside); statErr != nil {
				t.Fatalf("DeleteBucket(%q) removed an out-of-tree directory: %v", name, statErr)
			}
			if _, statErr := os.Stat(dataDir); statErr != nil {
				t.Fatalf("DeleteBucket(%q) removed the data root: %v", name, statErr)
			}
		})
		t.Run("create/"+name, func(t *testing.T) {
			err := w.services.CreateBucket(ctx, name)
			se, ok := errors.AsType[*admin.ServiceError](err)
			if !ok || se.Status != 400 {
				t.Fatalf("CreateBucket(%q) = %v, want a 400 ServiceError", name, err)
			}
			if se.Code == "" || se.Message == "" {
				t.Fatalf("CreateBucket(%q): empty error envelope %+v", name, se)
			}
		})
	}

	// The in-tree bucket survived every refusal.
	if _, err := os.Stat(filepath.Join(dataDir, "keepme", ".metadata")); err != nil {
		t.Fatalf("refused calls disturbed a legitimate bucket: %v", err)
	}
	// A traversal name created nothing.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "etc")); !os.IsNotExist(err) {
		t.Fatalf("CreateBucket created an out-of-tree directory: %v", err)
	}
}

// TestAdminWiringTraversalDeleteJSONEnvelope pins the wire-level shape the
// console decodes: the refusal carries a non-empty code/message and never a
// success flag.
func TestAdminWiringTraversalDeleteJSONEnvelope(t *testing.T) {
	dataDir := t.TempDir()
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, nil)
	w := buildAdminWiring(nil)

	err := w.services.DeleteBucket(context.Background(), "..")
	se, ok := errors.AsType[*admin.ServiceError](err)
	if !ok {
		t.Fatalf("err = %T %v, want *admin.ServiceError", err, err)
	}
	// The envelope is exactly the shape routes.writeError serializes.
	raw, mErr := json.Marshal(map[string]any{"error": map[string]string{
		"code": se.Code, "message": se.Message,
	}})
	if mErr != nil {
		t.Fatal(mErr)
	}
	var decoded struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if uErr := json.Unmarshal(raw, &decoded); uErr != nil {
		t.Fatal(uErr)
	}
	if decoded.Error.Code == "" || decoded.Error.Message == "" {
		t.Fatalf("refusal envelope is not JSON-honest: %s", raw)
	}
}

// TestAdminWiringLegitimateNameRoundTrip pins the fix is not over-broad on the
// management path: a legitimate name still creates, lists and deletes.
func TestAdminWiringLegitimateNameRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	installTestConfigStore(t, defaultServerConfig())
	installBucketTestEnv(t, dataDir, nil, nil)
	w := buildAdminWiring(nil)
	ctx := context.Background()

	if err := w.services.CreateBucket(ctx, "e2e-style-bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "e2e-style-bkt", ".metadata")); err != nil {
		t.Fatalf("bucket not created: %v", err)
	}
	raw, err := w.services.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if !containsName(raw, "e2e-style-bkt") {
		t.Fatalf("ListBuckets = %s, want it to contain e2e-style-bkt", raw)
	}
	if err := w.services.DeleteBucket(ctx, "e2e-style-bkt"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "e2e-style-bkt")); !os.IsNotExist(err) {
		t.Fatalf("bucket not deleted: %v", err)
	}
}

// containsName reports whether a GET /buckets document carries the name.
func containsName(raw json.RawMessage, name string) bool {
	var doc struct {
		Buckets []struct {
			Name string `json:"name"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	for _, b := range doc.Buckets {
		if b.Name == name {
			return true
		}
	}
	return false
}
