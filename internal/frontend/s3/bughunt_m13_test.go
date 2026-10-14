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
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
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
