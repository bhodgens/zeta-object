package admin

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/bucketmanager"
	"github.com/bhodgens/zeta-object/internal/fslock"
)

// bucketNameGuardEnv builds a frontend whose bucket services are the REAL
// bucketmanager entry points rooted at a temp data root — the same wiring
// admin_wiring.go installs in production (AllowDatasetDestroy false). Using the
// real functions here means this test pins the whole admin surface → manager
// path, not a stub's idea of it.
func bucketNameGuardEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	dataDir := t.TempDir()
	bucketmanager.Install(bucketmanager.Env{
		Locks:      fslock.Default,
		BucketPath: func(b string) string { return filepath.Join(dataDir, b) },
		Custom:     func(string) (string, bool) { return "", false },
	})
	t.Cleanup(func() { bucketmanager.Install(bucketmanager.Env{}) })

	svc := Services{
		CreateBucket: func(ctx context.Context, name string) error {
			return bucketmanager.Create(ctx, name)
		},
		DeleteBucket: func(ctx context.Context, name string) error {
			return bucketmanager.Delete(ctx, name, bucketmanager.DeleteOptions{AllowDatasetDestroy: false})
		},
	}
	return newRouteEnv(t, svc, nil), dataDir
}

// TestAdminRoutesTraversalBucketNameRefused is the admin-surface half of the
// H3 fix: the console's DELETE /api/buckets/{name} reaches this route with
// url.PathEscape("..") untouched ('.' is RFC 3986 unreserved), so classify
// handed ".." straight to the service. The answer must be an honest JSON 400
// naming the rule — never a 200 {"deleted":true}, never a 500 — and the
// out-of-tree directory must still be on disk afterwards.
func TestAdminRoutesTraversalBucketNameRefused(t *testing.T) {
	env, dataDir := bucketNameGuardEnv(t)

	// A directory OUTSIDE the data root: what "../srvdata" resolved to.
	outside := filepath.Join(filepath.Dir(dataDir), "srvdata")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	// A real bucket inside the data root, so the root is non-empty.
	if rr := serve(t, env, http.MethodPost, "/buckets", `{"name":"keepme"}`); rr.Code != http.StatusOK {
		t.Fatalf("setup POST /buckets: status = %d, body %s", rr.Code, rr.Body)
	}

	// ".." and "." DO route (classify only requires a single path segment), so
	// they must get the name refusal: a 400 from the service. "a/b" never
	// reaches a service at all — classify rejects a multi-segment name as an
	// unroutable path (404) — which is an equally honest refusal, so it is
	// asserted separately below.
	for _, name := range []string{"..", "."} {
		t.Run("delete/"+name, func(t *testing.T) {
			rr := serve(t, env, http.MethodDelete, "/buckets/"+name, "")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("DELETE /buckets/%s: status = %d, want 400 (body %s)", name, rr.Code, rr.Body)
			}
			body := mustJSON[errorEnvelope](t, rr)
			if body.Error.Code == "" || body.Error.Message == "" {
				t.Fatalf("empty error envelope: %+v", body.Error)
			}
			if !strings.Contains(body.Error.Message, "bucket name") {
				t.Fatalf("message must name the rule, got %q", body.Error.Message)
			}
			if _, err := os.Stat(outside); err != nil {
				t.Fatalf("DELETE /buckets/%s removed an out-of-tree directory: %v", name, err)
			}
			if _, err := os.Stat(dataDir); err != nil {
				t.Fatalf("DELETE /buckets/%s removed the data root: %v", name, err)
			}
		})
	}

	// A name carrying a separator is refused at the route, with an honest JSON
	// error and no filesystem effect.
	t.Run("delete/a/b", func(t *testing.T) {
		rr := serve(t, env, http.MethodDelete, "/buckets/a/b", "")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("DELETE /buckets/a/b: status = %d, want 404 (body %s)", rr.Code, rr.Body)
		}
		if body := mustJSON[errorEnvelope](t, rr); body.Error.Code == "" {
			t.Fatalf("empty error envelope: %+v", body.Error)
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("DELETE /buckets/a/b removed an out-of-tree directory: %v", err)
		}
	})

	// No refusal anywhere produced a 500 or a success flag.
	t.Run("no 500 and no success flag", func(t *testing.T) {
		for _, name := range []string{"..", ".", "a/b", "srvdata"} {
			rr := serve(t, env, http.MethodDelete, "/buckets/"+name, "")
			if rr.Code >= 500 {
				t.Fatalf("DELETE /buckets/%s reported a server fault: %d %s", name, rr.Code, rr.Body)
			}
			if rr.Code == http.StatusOK {
				t.Fatalf("DELETE /buckets/%s reported success: %s", name, rr.Body)
			}
			if strings.Contains(rr.Body.String(), `"deleted":true`) {
				t.Fatalf("DELETE /buckets/%s claimed deletion: %s", name, rr.Body)
			}
		}
	})

	// The in-tree bucket survived every refusal.
	if _, err := os.Stat(filepath.Join(dataDir, "keepme", ".metadata")); err != nil {
		t.Fatalf("refused calls disturbed a legitimate bucket: %v", err)
	}
}

// TestAdminRoutesCreateTraversalBucketNameRefused is the POST half: a traversal
// name in the create body must be a 400 JSON refusal that creates nothing.
func TestAdminRoutesCreateTraversalBucketNameRefused(t *testing.T) {
	env, dataDir := bucketNameGuardEnv(t)
	parent := filepath.Dir(dataDir)

	for _, body := range []string{`{"name":"../escape"}`, `{"name":".."}`, `{"name":""}`, `{"name":"a/b"}`} {
		rr := serve(t, env, http.MethodPost, "/buckets", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("POST /buckets %s: status = %d, want 400 (body %s)", body, rr.Code, rr.Body)
		}
		envelope := mustJSON[errorEnvelope](t, rr)
		if envelope.Error.Code == "" || envelope.Error.Message == "" {
			t.Fatalf("POST /buckets %s: empty error envelope", body)
		}
	}

	// Nothing was created outside the root, and the root is intact.
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Fatalf(`POST /buckets {"name":"../escape"} created a directory outside the data root: %v`, err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data root disturbed: %v", err)
	}
}

// TestAdminRoutesLegitimateBucketNameRoundTrip pins the fix is not over-broad
// on the admin surface: a legitimate name still creates and deletes, and the
// success envelope is unchanged.
func TestAdminRoutesLegitimateBucketNameRoundTrip(t *testing.T) {
	env, dataDir := bucketNameGuardEnv(t)

	rr := serve(t, env, http.MethodPost, "/buckets", `{"name":"legit-bucket"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /buckets: status = %d, body %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "legit-bucket", ".metadata")); err != nil {
		t.Fatalf("bucket not created: %v", err)
	}

	rr = serve(t, env, http.MethodDelete, "/buckets/legit-bucket", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE /buckets/{name}: status = %d, body %s", rr.Code, rr.Body)
	}
	if got := mustJSON[map[string]any](t, rr); got["deleted"] != true {
		t.Fatalf("delete envelope = %s, want deleted:true", rr.Body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "legit-bucket")); !os.IsNotExist(err) {
		t.Fatalf("bucket not deleted: %v", err)
	}
}
