// batch_principal_test.go — the LIVE-HARNESS-found defect: a JSON ?batch
// copy arriving over the WEBDAV mount must stamp the requesting
// principal's forensic attribution breadcrumbs (user.zeta.owner set once
// at create, user.zeta.writer.<principal> per writer), exactly like the
// same copy over the s3 mount.
//
// Why this is its own test rather than another parity assert: the shared
// batch executor (internal/frontend/s3/batch_endpoint.go) resolves the
// principal from the REQUEST CONTEXT. A frontend that publishes the
// authenticated identity under its OWN private context key makes the
// executor miss it, fall back to the wildcard principal, and write
// owner='unauthenticated' on every batch-written object — while every
// existing test stayed green because each test published and read under
// the SAME private key. The live ZFS harness found it on the h3 mount
// (which wraps this very handler): s15's breadcrumb check failed there
// while the s3 mount's twin passed.
//
// The pins here:
//   - the mount drives the REAL Basic-authenticated webdav pipeline (the
//     production auth shape: frontends.go wires NewBasicAuthenticator over
//     the identity registry) — never a stub that publishes its own key;
//   - the breadcrumb is read back from the real fsbackend xattr, so the
//     assertion is the on-disk contract, not a production helper.
package webdav

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/batchops"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// davXattr reads one xattr by name from a path (the test-side raw read,
// so the assertion pins the on-disk contract). ok=false means absent.
func davXattr(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test-side read-only handle.
	size, err := unix.Fgetxattr(int(f.Fd()), name, nil)
	if err != nil {
		return "", false // absent (ENOATTR/ENODATA) or unreadable: not stamped
	}
	buf := make([]byte, size)
	n, err := unix.Fgetxattr(int(f.Fd()), name, buf)
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

// TestWebdavBatch_StampsRequesterPrincipalBreadcrumb: the exact live
// failure as a unit test — a batch copy over webdav records the REAL
// requester, never the wildcard fallback.
func TestWebdavBatch_StampsRequesterPrincipalBreadcrumb(t *testing.T) {
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucket := "dav-crumb-bkt"
	if err := os.MkdirAll(filepath.Join(dataDir, bucket, ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	be := s3.TestBackend()
	if be == nil {
		t.Fatal("no backend installed by SetupVersioningTestEnv")
	}
	const src = "src/plain.txt"
	if _, err := be.Put(t.Context(), bucket, src, strings.NewReader("payload"), int64(len("payload")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	// The PRODUCTION auth shape: Basic over the identity registry
	// (frontends.go wires exactly this for the webdav mount).
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "dav-user", AccessKey: "ak-dav", SecretKey: "sk-dav"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(be, Config{Bucket: bucket}, WithAuthenticator(auth.NewBasicAuthenticator(reg)),
		WithBucketPathResolver(func(b string) string { return filepath.Join(dataDir, b) }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}

	manifest := `{"operations":[` +
		`{"op":"copy","from":"` + src + `","to":"dst/plain.txt"},` +
		`{"op":"move","from":"` + src + `","to":"dst/moved.txt"}]}`
	req := httptest.NewRequest("POST", "/?batch", strings.NewReader(manifest))
	req.SetBasicAuth("ak-dav", "sk-dav")
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("batch status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp batchops.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal batch response: %v (%s)", err, w.Body.String())
	}
	for i, r := range resp.Results {
		if r.Status != batchops.StatusOK {
			t.Fatalf("results[%d] = %+v, want ok", i, r)
		}
	}

	for _, key := range []string{"dst/plain.txt", "dst/moved.txt"} {
		path := filepath.Join(dataDir, bucket, key)
		owner, ok := davXattr(t, path, "user.zeta.owner")
		if !ok || owner != "ak-dav" {
			t.Errorf("%s user.zeta.owner = %q ok=%v, want ak-dav — a batch copy over webdav must stamp the REAL requester, not the wildcard fallback", key, owner, ok)
		}
		writer, ok := davXattr(t, path, "user.zeta.writer.ak-dav")
		if !ok {
			t.Errorf("%s has no user.zeta.writer.ak-dav breadcrumb", key)
			continue
		}
		if !strings.HasPrefix(writer, "put@") {
			t.Errorf("%s user.zeta.writer.ak-dav = %q, want put@<RFC3339>", key, writer)
		}
	}
}
