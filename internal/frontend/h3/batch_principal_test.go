// batch_principal_test.go — the h3 (HTTP/3) mount's half of the
// shared-identity fix.
//
// The h3 frontend WRAPS a webdav frontend (frontend.go builds it with the
// same constructor options the webdav factory entry uses), so ?batch over
// QUIC runs the same executor. That inheritance is exactly what made the
// live ZFS harness's s15 check fail while the unit suite was green: the h3
// mount inherits the wrapped webdav's PRIVATE identity context key, so
// the shared s3 batch executor never saw the principal and stamped
// owner='unauthenticated' on every batch-written object.
//
// This test drives the REAL HTTP/3 server on loopback with a real quic-go
// client presenting a real client certificate (the mTLS production
// posture), POSTs a batch manifest, and reads the breadcrumb back from the
// on-disk xattr. The frontend is constructed the way frontends.go wires it
// — New over the generated PKI, the cert registry, and the bucket-path
// resolver the same getBucketPath function production installs.
package h3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
	"golang.org/x/sys/unix"
)

// h3Xattr reads one xattr by name (the test-side raw read, so the
// assertion pins the on-disk contract). ok=false means absent.
func h3Xattr(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // test-side read-only handle.
	size, err := unix.Fgetxattr(int(f.Fd()), name, nil)
	if err != nil {
		return "", false
	}
	buf := make([]byte, size)
	n, err := unix.Fgetxattr(int(f.Fd()), name, buf)
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

// TestServe_BatchCopyStampsRequesterPrincipalBreadcrumb: a ?batch copy
// arriving over QUIC must attribute the destination to the certificate's
// principal (the CN), NOT to the wildcard fallback. This is the live
// harness's s15 finding as a unit test, on the real transport.
func TestServe_BatchCopyStampsRequesterPrincipalBreadcrumb(t *testing.T) {
	env := newH3TestEnv(t)

	// The h3 frontend's batch executor resolves object paths through the
	// SHARED s3 side, so the test needs the s3 package seams installed
	// (a real fsbackend under a temp root) exactly as production does:
	// SetupVersioningTestEnv installs the config view, the fs-root resolver
	// and the backend lookup the executor drives.
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucket := "photos"
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

	f, err := New(be, env.validConfig(), env.registry, func(b string) string {
		return filepath.Join(dataDir, b)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv, err := f.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})

	client := h3Client(t, env, srv.Addr().String(), true)
	manifest := `{"operations":[{"op":"copy","from":"` + src + `","to":"dst/plain.txt"}]}`
	url := fmt.Sprintf("https://%s/%s?batch", srv.Addr().String(), bucket)
	resp, err := client.Post(url, "application/json", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("POST ?batch over h3: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read ?batch response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?batch status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"status":"ok"`)) {
		t.Fatalf("batch item did not report ok: %s", body)
	}

	path := filepath.Join(dataDir, bucket, "dst/plain.txt")
	owner, ok := h3Xattr(t, path, "user.zeta.owner")
	if !ok || owner != "device-1" {
		t.Errorf("h3 batch copy user.zeta.owner = %q ok=%v, want device-1 — the wrapped webdav must publish the identity under the SHARED context key so the batch executor sees the real principal", owner, ok)
	}
	writer, ok := h3Xattr(t, path, "user.zeta.writer.device-1")
	if !ok {
		t.Errorf("h3 batch copy has no user.zeta.writer.device-1 breadcrumb")
	} else if !strings.HasPrefix(writer, "put@") {
		t.Errorf("h3 batch copy user.zeta.writer.device-1 = %q, want put@<RFC3339>", writer)
	}
}
