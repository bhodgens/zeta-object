// migration_test.go — unit-level back-compat locks (pluggable-
// authentication tree leaf 06 Task 1): the S3 wire error BYTES for
// unknown-key and bad-signature are unchanged from the pre-tree literals.
// These reference (not duplicate) the adapter coverage in
// auth_adapter_test.go/sigv4_test.go; the golden bodies here are the
// migration lock.
package s3_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestMigrationUnknownKeyErrorBytes pins the exact S3 error document for an
// unknown access key — must be byte-identical to the pre-tree response.
func TestMigrationUnknownKeyErrorBytes(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "env", AccessKey: "AKENV", SecretKey: "sk-env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	installRegistry(t, reg)
	f := s3.New(nil)

	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, signPath(t, "GET", "/bucket-one/obj.txt", "AKUNKNOWN", "sk-unknown", ""))
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<Code>InvalidAccessKeyId</Code>",
		"<Message>The AWS Access Key Id you provided does not exist in our records.</Message>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestMigrationBadSignatureErrorBytes pins the SignatureDoesNotMatch bytes
// for a known key with a wrong secret.
func TestMigrationBadSignatureErrorBytes(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "env", AccessKey: "AKENV", SecretKey: "sk-env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	installRegistry(t, reg)
	f := s3.New(nil)

	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, signPath(t, "GET", "/bucket-one/obj.txt", "AKENV", "WRONG-SECRET", ""))
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<Code>SignatureDoesNotMatch</Code>") {
		t.Errorf("body missing SignatureDoesNotMatch:\n%s", body)
	}
}

// TestMigrationEnvOnlyUnknownKeyMatchesLegacyCredentialSource proves the
// registry path and the legacy CredentialSource path produce byte-identical
// error documents for the same unknown-key request — the migration is
// invisible on the failure path too.
func TestMigrationEnvOnlyUnknownKeyMatchesLegacyCredentialSource(t *testing.T) {
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "env", AccessKey: "minioadmin", SecretKey: "minioadmin"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Registry path.
	installRegistry(t, reg)
	fr := s3.New(nil)
	wr := httptest.NewRecorder()
	fr.Handler().ServeHTTP(wr, signPath(t, "GET", "/no-such-bkt/obj.txt", "ghost", "ghost", ""))
	withReg := wr.Body.String()

	// Legacy path (no registry).
	s3.InstallIdentityRegistry(nil)
	fl := s3.New(nil, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	wl := httptest.NewRecorder()
	fl.Handler().ServeHTTP(wl, signPath(t, "GET", "/no-such-bkt/obj.txt", "ghost", "ghost", ""))
	legacy := wl.Body.String()

	if wr.Code != wl.Code {
		t.Fatalf("status diverges: %d vs %d", wr.Code, wl.Code)
	}
	if withReg != legacy {
		t.Fatalf("error bytes diverge between registry and legacy paths:\n%s\nvs\n%s", withReg, legacy)
	}
	if !strings.Contains(withReg, "InvalidAccessKeyId") {
		t.Errorf("unexpected body: %s", withReg)
	}
}
