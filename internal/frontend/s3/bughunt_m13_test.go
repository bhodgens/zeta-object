// bughunt_m13_test.go — M13 regression pin: POST ?batch and POST ?delete
// are batch sub-resources, not creates, so a create-only rich grant must
// NOT authorize them (they execute writes and deletes). The op adapter
// (opForMethod) must require write for ?batch and delete for ?delete
// while plain bucket-root POST (CreateBucket, multipart initiate) keeps
// OpCreate.
package s3_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// m13Serve drives the signed request through the full pipeline and
// returns the recorder (serves the handler; recorder pattern from
// dispatch_grants_test.go).
func m13Serve(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// TestBughuntM13_BatchSubresourcesNotCreate drives the FULL pipeline
// (SigV4 sign → serveHTTP) with a rich grant that allows create but
// denies write and delete on the bucket: POST ?batch and POST ?delete
// must answer 403 AccessDenied instead of executing manifest items.
func TestBughuntM13_BatchSubresourcesNotCreate(t *testing.T) {
	createOnly, err := auth.ParseGrantValue("m13-test", "m13-bucket", json.RawMessage(`{"ops":["create"]}`))
	if err != nil {
		t.Fatalf("grant fixture: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "creator", AccessKey: "AKM13", SecretKey: "sk-m13", Grants: map[string]json.RawMessage{
			"m13-bucket": m13GrantJSON(t, createOnly),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	handler := srv.Config.Handler

	// Create the bucket — the one op the create grant DOES allow.
	if w := m13Serve(t, handler, signPath(t, "PUT", "/m13-bucket", "AKM13", "sk-m13", "")); w.Code >= 400 {
		t.Fatalf("create bucket (allowed by create grant): %d %s", w.Code, w.Body.String())
	}

	// Sanity: the create-only identity must be DENIED a plain object PUT
	// — proves the grant shape denies write before the batch assertions.
	if w := m13Serve(t, handler, signPath(t, "PUT", "/m13-bucket/victim.txt", "AKM13", "sk-m13", "data")); w.Code != http.StatusForbidden {
		t.Fatalf("create-only identity wrote an object: %d %s (grant shape broken)", w.Code, w.Body.String())
	}

	// POST ?batch (manifest items write AND delete) → 403 AccessDenied.
	req := signPath(t, "POST", "/m13-bucket?batch", "AKM13", "sk-m13", `{"operations":[{"op":"delete","from":"victim.txt"}]}`)
	if w := m13Serve(t, handler, req); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Fatalf("POST ?batch as create-only = %d %s, want 403 AccessDenied (M13)", w.Code, w.Body.String())
	}

	// POST ?delete → 403 AccessDenied.
	req = signPath(t, "POST", "/m13-bucket?delete", "AKM13", "sk-m13", `<Delete><Object><Key>victim.txt</Key></Object></Delete>`)
	if w := m13Serve(t, handler, req); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Fatalf("POST ?delete as create-only = %d %s, want 403 AccessDenied (M13)", w.Code, w.Body.String())
	}

	// Multipart initiate (plain POST with an object key) keeps the WRITE
	// classification → 403 for this identity too (parity pin: nothing
	// regressed into create).
	req = signPath(t, "POST", "/m13-bucket/big.bin?uploads", "AKM13", "sk-m13", "")
	if w := m13Serve(t, handler, req); w.Code != http.StatusForbidden {
		t.Fatalf("multipart initiate as create-only = %d, want 403 (write-class)", w.Code)
	}
}

// TestBughuntF1_DeleteBatchDoubleParamRejected pins the post-wave F1 fix:
// POST /bucket?delete&batch (BOTH sub-resource params) is malformed — no
// legitimate client sends both — and used to be an authz/dispatch
// precedence mismatch (opForMethod: ?batch→OpWrite checked first; dispatch:
// ?delete routed first), so a WRITE-ONLY grant could execute DeleteObjects
// by appending both params. The dispatch now rejects the combination 400
// InvalidArgument BEFORE any handler runs. Plain ?delete (needs OpDelete)
// and plain ?batch (needs OpWrite) keep their M13 pins above.
func TestBughuntF1_DeleteBatchDoubleParamRejected(t *testing.T) {
	writeOnly, err := auth.ParseGrantValue("f1-test", "f1-bucket", json.RawMessage(`{"ops":["write"]}`))
	if err != nil {
		t.Fatalf("grant fixture: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "writer", AccessKey: "AKF1", SecretKey: "sk-f1", Grants: map[string]json.RawMessage{
			"f1-bucket": m13GrantJSON(t, writeOnly),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	handler := srv.Config.Handler

	// Create the bucket DIRECTORY directly (mkdir) — the bucket must
	// exist, but a write-only grant cannot PUT /bucket (OpCreate), and
	// granting create would weaken the escalation fixture. newTestServer
	// (via newRegistryTestServer) exposes its backend root through the
	// installed config view's DataDir.
	if err := os.MkdirAll(filepath.Join(strings.TrimSuffix(s3.InstalledDataDir(), "/"), "f1-bucket"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Sanity: the write-only identity CAN write (grant shape proves the
	// escalation below is real, not a broken fixture).
	if w := m13Serve(t, handler, signPath(t, "PUT", "/f1-bucket/own.txt", "AKF1", "sk-f1", "data")); w.Code >= 400 {
		t.Fatalf("write-only identity PUT (allowed): %d %s", w.Code, w.Body.String())
	}

	// POST ?delete&batch as write-only → 400 InvalidArgument, NOT a
	// DeleteObjects execution under the OpWrite classification.
	req := signPath(t, "POST", "/f1-bucket?delete&batch", "AKF1", "sk-f1",
		`<Delete><Object><Key>victim.txt</Key></Object></Delete>`)
	if w := m13Serve(t, handler, req); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("POST ?delete&batch as write-only = %d %s, want 400 InvalidArgument (F1)", w.Code, w.Body.String())
	}

	// Plain ?delete still needs OpDelete → 403 for this identity (the
	// double-param rejection must not widen or narrow the single-param
	// classifications).
	req = signPath(t, "POST", "/f1-bucket?delete", "AKF1", "sk-f1",
		`<Delete><Object><Key>victim.txt</Key></Object></Delete>`)
	if w := m13Serve(t, handler, req); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Fatalf("POST ?delete as write-only = %d %s, want 403 AccessDenied (plain ?delete keeps OpDelete)", w.Code, w.Body.String())
	}
}

// TestBughuntM13_WriteGrantStillAuthorizesBatch pins the positive side: a
// grant with write+delete DOES authorize both batch sub-resources — the
// fix tightens create→write/delete, it does not break the write path.
func TestBughuntM13_WriteGrantStillAuthorizesBatch(t *testing.T) {
	fullOps, err := auth.ParseGrantValue("m13-test", "m13b-bucket", json.RawMessage(`{"ops":["read","write","list","delete","create"]}`))
	if err != nil {
		t.Fatalf("grant fixture: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "writer", AccessKey: "AKM13B", SecretKey: "sk-m13b", Grants: map[string]json.RawMessage{
			"m13b-bucket": m13GrantJSON(t, fullOps),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newRegistryTestServer(t, reg)
	handler := srv.Config.Handler

	if w := m13Serve(t, handler, signPath(t, "PUT", "/m13b-bucket", "AKM13B", "sk-m13b", "")); w.Code >= 400 {
		t.Fatalf("create bucket: %d %s", w.Code, w.Body.String())
	}
	if w := m13Serve(t, handler, signPath(t, "PUT", "/m13b-bucket/k.txt", "AKM13B", "sk-m13b", "data")); w.Code >= 400 {
		t.Fatalf("put object: %d %s", w.Code, w.Body.String())
	}

	// POST ?delete executes (NOT 403).
	req := signPath(t, "POST", "/m13b-bucket?delete", "AKM13B", "sk-m13b", `<Delete><Object><Key>k.txt</Key></Object></Delete>`)
	if w := m13Serve(t, handler, req); w.Code == http.StatusForbidden {
		t.Fatalf("POST ?delete with write+delete grant = 403: %s", w.Body.String())
	}

	// POST ?batch executes (NOT 403).
	req = signPath(t, "POST", "/m13b-bucket?batch", "AKM13B", "sk-m13b", `{"operations":[{"op":"delete","from":"gone.txt"}]}`)
	if w := m13Serve(t, handler, req); w.Code == http.StatusForbidden {
		t.Fatalf("POST ?batch with write+delete grant = 403: %s", w.Body.String())
	}
}

// m13GrantJSON re-serializes a parsed GrantExpr into the registry config
// form (round-trip through the same parser the registry build uses).
func m13GrantJSON(t *testing.T, e auth.GrantExpr) json.RawMessage {
	t.Helper()
	ops := make([]string, 0, len(e.Ops))
	for _, op := range []auth.Op{"read", "write", "list", "delete", "create"} {
		if e.Ops[op] {
			ops = append(ops, string(op))
		}
	}
	raw, err := json.Marshal(struct {
		Ops []string `json:"ops"`
	}{Ops: ops})
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	return raw
}
