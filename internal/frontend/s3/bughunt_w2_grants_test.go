// bughunt_w2_grants_test.go — regression pin for W2-M1 (s3 ?batch mount):
// POST ?batch executes put/copy AND delete manifest items but the executor
// runs no per-item grant check, so a WRITE-ONLY grant could delete through
// the batch mount. The mount now requires the identity to hold write AND
// delete; read-only still 403s.
package s3_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// w2GrantsFor parses a rich grant expression for one bucket and re-encodes
// it in the registry's config form (the m13GrantJSON shape, local copy).
func w2GrantsFor(t *testing.T, name, bucket, ops string) map[string]json.RawMessage {
	t.Helper()
	expr, err := auth.ParseGrantValue(name, bucket, json.RawMessage(`{"ops":[`+ops+`]}`))
	if err != nil {
		t.Fatalf("grant fixture %s (%s): %v", name, ops, err)
	}
	opsList := make([]string, 0, len(expr.Ops))
	for _, op := range []auth.Op{"read", "write", "list", "delete", "create"} {
		if expr.Ops[op] {
			opsList = append(opsList, string(op))
		}
	}
	raw, err := json.Marshal(struct {
		Ops []string `json:"ops"`
	}{Ops: opsList})
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	return map[string]json.RawMessage{bucket: raw}
}

// TestW2M1_S3BatchRequiresWriteAndDelete pins the s3 ?batch mount gate:
// write-only 403 (a manifest can delete), write+delete passes, read-only
// still 403.
func TestW2M1_S3BatchRequiresWriteAndDelete(t *testing.T) {
	const bucket = "w2m1-s3-bucket"
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "writer", AccessKey: "AKW2M1W", SecretKey: "sk-w2m1w",
			Grants: w2GrantsFor(t, "writer", bucket, `"write"`)},
		{Name: "fuller", AccessKey: "AKW2M1F", SecretKey: "sk-w2m1f",
			Grants: w2GrantsFor(t, "fuller", bucket, `"read","write","list","delete","create"`)},
		{Name: "reader", AccessKey: "AKW2M1R", SecretKey: "sk-w2m1r",
			Grants: w2GrantsFor(t, "reader", bucket, `"read","list"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	handler := srv.Config.Handler

	// The bucket directory exists without the create op (a rich grant
	// without "create" cannot PUT /bucket — same fixture shape as the F1
	// pin).
	if err := os.MkdirAll(filepath.Join(strings.TrimSuffix(s3.InstalledDataDir(), "/"), bucket), 0o755); err != nil {
		t.Fatal(err)
	}

	batchBody := `{"operations":[{"op":"delete","from":"victim.txt"}]}`

	// Sanity: the writer CAN write an object (the grant shape proves the
	// escalation below is real).
	if w := m13Serve(t, handler, signPath(t, "PUT", "/"+bucket+"/own.txt", "AKW2M1W", "sk-w2m1w", "data")); w.Code >= 400 {
		t.Fatalf("write-only PUT (allowed): %d %s", w.Code, w.Body.String())
	}

	// W2-M1 core: write-only on ?batch → 403 (the manifest may delete).
	if w := m13Serve(t, handler, signPath(t, "POST", "/"+bucket+"?batch", "AKW2M1W", "sk-w2m1w", batchBody)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Fatalf("POST ?batch as write-only = %d %s, want 403 AccessDenied (W2-M1: a manifest can delete)", w.Code, w.Body.String())
	}

	// Read-only stays 403 (the write half denies it first — unchanged).
	if w := m13Serve(t, handler, signPath(t, "POST", "/"+bucket+"?batch", "AKW2M1R", "sk-w2m1r", batchBody)); w.Code != http.StatusForbidden {
		t.Fatalf("POST ?batch as read-only = %d %s, want 403", w.Code, w.Body.String())
	}

	// write+delete passes (NOT 403).
	if w := m13Serve(t, handler, signPath(t, "POST", "/"+bucket+"?batch", "AKW2M1F", "sk-w2m1f", batchBody)); w.Code == http.StatusForbidden {
		t.Fatalf("POST ?batch with write+delete = 403: %s (W2-M1 must not over-tighten)", w.Body.String())
	}
}
