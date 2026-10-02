// lockhandler_test.go — leaf 03 (webdav-locking-2026-10): wire-level LOCK/
// UNLOCK behavior and write-path enforcement over a lock-enabled frontend
// (WithLockStoreRoot wired at a t.TempDir). Covers the brief's matrix:
// LOCK 200 + Lock-Token + lockdiscovery body, conflicting LOCK 423,
// Depth-infinity 400, shared-scope 400, timeout cap, UNLOCK 204/409,
// PUT/DELETE/MOVE without a token 423, with the If token pass, and lock
// management being exempt from enforcement.
package webdav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLockingFrontend builds a stub-backend frontend with the lock store
// rooted at a temp dir (bucket → dir mapping is identity for "photos").
func newLockingFrontend(t *testing.T, cfg Config) (*Frontend, *stubBackend) {
	t.Helper()
	root := t.TempDir()
	be := newStubBackend()
	f, err := New(be, cfg, WithAuthenticator(newStubAuth()), WithLockStoreRoot(func(string) string { return root }))
	if err != nil {
		t.Fatal(err)
	}
	return f, be
}

// lockBody is the RFC 4918 §9.10.6 LOCK request body (exclusive write,
// owner href).
const lockBody = `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner><D:href>mailto:alice@example.com</D:href></D:owner></D:lockinfo>`

// serve issues a request against f and returns the recorder.
func serve(f *Frontend, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	return rec
}

// lockResult carries the pieces LOCK tests assert on.
type lockResult struct {
	status int
	token  string // Lock-Token header, angle brackets stripped
	body   string
}

// doLOCK sends a LOCK and extracts the token.
func doLOCK(t *testing.T, f *Frontend, path, body, depth, timeout string) lockResult {
	t.Helper()
	req := httptest.NewRequest("LOCK", path, strings.NewReader(body))
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	if timeout != "" {
		req.Header.Set("Timeout", timeout)
	}
	rec := serve(f, req)
	return lockResult{
		status: rec.Code,
		token:  strings.Trim(strings.TrimSpace(rec.Header().Get("Lock-Token")), "<>"),
		body:   rec.Body.String(),
	}
}

func TestLOCK_HappyPath(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	res := doLOCK(t, f, "/a.txt", lockBody, "0", "Second-600")
	if res.status != 200 {
		t.Fatalf("status = %d, want 200; body=%.200s", res.status, res.body)
	}
	if !strings.HasPrefix(res.token, "opaquelocktoken:") {
		t.Fatalf("Lock-Token = %q, want an opaquelocktoken: URI", res.token)
	}
	for _, want := range []string{"<D:lockdiscovery>", "<D:locktype><D:write/>", "<D:lockscope><D:exclusive/>", "<D:depth>0</D:depth>", "<D:href>mailto:alice@example.com</D:href>", res.token, "<D:lockroot><D:href>/a.txt</D:href>"} {
		if !strings.Contains(res.body, want) {
			t.Fatalf("lockdiscovery body missing %q:\n%s", want, res.body)
		}
	}
	if ct := serve(f, httptest.NewRequest("LOCK", "/zz-unused", nil)).Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		// (content-type is asserted on the real response below)
		_ = ct
	}
	req := httptest.NewRequest("LOCK", "/b.txt", strings.NewReader(lockBody))
	req.Header.Set("Timeout", "Second-120")
	rec := serve(f, req)
	if !strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		t.Fatalf("LOCK Content-Type = %q, want xml", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "Second-120") {
		t.Fatalf("LOCK body missing granted timeout Second-120:\n%s", rec.Body.String())
	}
}

func TestLOCK_Conflict423(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	first := doLOCK(t, f, "/a.txt", lockBody, "", "")
	if first.status != 200 {
		t.Fatalf("first LOCK: status = %d, want 200", first.status)
	}
	rec := serve(f, httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(lockBody)))
	if rec.Code != http.StatusLocked {
		t.Fatalf("second LOCK: status = %d, want 423; body=%.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<D:error") {
		t.Fatalf("423 body missing <D:error>:\n%s", rec.Body.String())
	}
}

func TestLOCK_DepthInfinity400(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	req := httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(lockBody))
	req.Header.Set("Depth", "infinity")
	rec := serve(f, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Depth infinity LOCK: status = %d, want 400 (documented v1 limit)", rec.Code)
	}
}

func TestLOCK_SharedScope400(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	body := `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:shared/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	rec := serve(f, httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("shared lockscope: status = %d, want 400 (documented v1 limit)", rec.Code)
	}
}

func TestLOCK_TimeoutCap3600(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	// Asking for a year gets the 3600s cap — in the granted-lease
	// rendered back in the lockdiscovery body.
	req := httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(lockBody))
	req.Header.Set("Timeout", "Second-31536000")
	rec := serve(f, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Second-3600") {
		t.Fatalf("body missing capped Second-3600:\n%s", rec.Body.String())
	}
	// "Infinite" gets the 600s default, not the cap.
	req = httptest.NewRequest("LOCK", "/b.txt", strings.NewReader(lockBody))
	req.Header.Set("Timeout", "Infinite")
	rec = serve(f, req)
	if !strings.Contains(rec.Body.String(), "Second-600") {
		t.Fatalf("Infinite timeout: want default Second-600:\n%s", rec.Body.String())
	}
}

func TestLOCK_Collection405(t *testing.T) {
	// The davfs2 directory-lock open question (master.md) resolves to
	// "v1 files only, documented": collections and the root stay 405.
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	for _, path := range []string{"/", "/dir/"} {
		rec := serve(f, httptest.NewRequest("LOCK", path, strings.NewReader(lockBody)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("LOCK %s: status = %d, want 405 (v1 locks files only)", path, rec.Code)
		}
		rec = serve(f, httptest.NewRequest("UNLOCK", path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("UNLOCK %s: status = %d, want 405 (v1 locks files only)", path, rec.Code)
		}
	}
}

func TestLOCK_WithoutStoreRoot405(t *testing.T) {
	// No WithLockStoreRoot: the pre-locking 405 behavior is preserved
	// (locks are opt-in via wiring).
	f, _ := newTestFrontend(Config{Bucket: "photos"})
	rec := serve(f, httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(lockBody)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("LOCK without lock root: status = %d, want 405", rec.Code)
	}
	rec = serve(f, httptest.NewRequest("UNLOCK", "/a.txt", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("UNLOCK without lock root: status = %d, want 405", rec.Code)
	}
}

func TestUNLOCK_CorrectToken204_WrongToken409(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	res := doLOCK(t, f, "/a.txt", lockBody, "", "")
	if res.status != 200 {
		t.Fatalf("LOCK: status = %d, want 200", res.status)
	}
	// Wrong token → 409 Conflict (the brief's pin).
	req := httptest.NewRequest("UNLOCK", "/a.txt", nil)
	req.Header.Set("Lock-Token", "<opaquelocktoken:00000000-0000-4000-8000-000000000000>")
	if rec := serve(f, req); rec.Code != http.StatusConflict {
		t.Fatalf("UNLOCK wrong token: status = %d, want 409", rec.Code)
	}
	// Missing token → 400.
	req = httptest.NewRequest("UNLOCK", "/a.txt", nil)
	if rec := serve(f, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("UNLOCK missing token: status = %d, want 400", rec.Code)
	}
	// Correct token → 204, and the lock is gone (a fresh LOCK succeeds).
	req = httptest.NewRequest("UNLOCK", "/a.txt", nil)
	req.Header.Set("Lock-Token", "<"+res.token+">")
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK correct token: status = %d, want 204", rec.Code)
	}
	if again := doLOCK(t, f, "/a.txt", lockBody, "", ""); again.status != 200 {
		t.Fatalf("LOCK after unlock: status = %d, want 200", again.status)
	}
	// UNLOCK on an unlocked resource → 409 (nothing to release).
	req = httptest.NewRequest("UNLOCK", "/gone.txt", nil)
	req.Header.Set("Lock-Token", "<"+res.token+">")
	if rec := serve(f, req); rec.Code != http.StatusConflict {
		t.Fatalf("UNLOCK unlocked resource: status = %d, want 409", rec.Code)
	}
}

func TestLockEnforcement_PUTWithoutToken423(t *testing.T) {
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "a.txt", []byte("old"))
	if res := doLOCK(t, f, "/a.txt", lockBody, "", ""); res.status != 200 {
		t.Fatalf("LOCK: status = %d, want 200", res.status)
	}
	// PUT without the token → 423, and the object is untouched.
	req := httptest.NewRequest("PUT", "/a.txt", strings.NewReader("new"))
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("PUT without token: status = %d, want 423", rec.Code)
	}
	if string(be.bodies["photos\x00a.txt"]) != "old" {
		t.Fatal("locked PUT reached the backend")
	}
	// DELETE without the token → 423.
	req = httptest.NewRequest("DELETE", "/a.txt", nil)
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("DELETE without token: status = %d, want 423", rec.Code)
	}
	if !be.hasKey("photos", "a.txt") {
		t.Fatal("locked DELETE reached the backend")
	}
}

func TestLockEnforcement_WithIfTokenPasses(t *testing.T) {
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "a.txt", []byte("old"))
	res := doLOCK(t, f, "/a.txt", lockBody, "", "")
	if res.status != 200 {
		t.Fatalf("LOCK: status = %d, want 200", res.status)
	}
	req := httptest.NewRequest("PUT", "/a.txt", strings.NewReader("new"))
	req.Header.Set("If", "(<"+res.token+">)")
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT with If token: status = %d, want 204; body=%.200s", rec.Code, rec.Body.String())
	}
	if string(be.bodies["photos\x00a.txt"]) != "new" {
		t.Fatal("token-carrying PUT did not land")
	}
	// Wrong token still 423s.
	req = httptest.NewRequest("PUT", "/a.txt", strings.NewReader("newer"))
	req.Header.Set("If", "(<opaquelocktoken:00000000-0000-4000-8000-000000000000>)")
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("PUT with wrong token: status = %d, want 423", rec.Code)
	}
}

func TestLockEnforcement_MoveAndCopyDestination(t *testing.T) {
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "src.txt", []byte("data"))
	be.seed("photos", "dst.txt", []byte("dst"))
	res := doLOCK(t, f, "/dst.txt", lockBody, "", "")
	if res.status != 200 {
		t.Fatalf("LOCK dst: status = %d, want 200", res.status)
	}
	// MOVE onto the locked destination without the token → 423.
	req := httptest.NewRequest("MOVE", "/src.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("MOVE onto locked dst: status = %d, want 423", rec.Code)
	}
	if be.hasKey("photos", "src.txt") == false || string(be.bodies["photos\x00dst.txt"]) != "dst" {
		t.Fatal("locked MOVE reached the backend")
	}
	// With the token → 204 (overwriting move).
	req = httptest.NewRequest("MOVE", "/src.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	req.Header.Set("If", "(<"+res.token+">)")
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE with token: status = %d, want 204; body=%.200s", rec.Code, rec.Body.String())
	}
	if be.hasKey("photos", "src.txt") {
		t.Fatal("token-carrying MOVE left the source behind")
	}
	// COPY onto a locked destination without the token → 423 (the If
	// header applies to the destination).
	be.seed("photos", "src2.txt", []byte("data2"))
	req = httptest.NewRequest("COPY", "/src2.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("COPY onto locked dst: status = %d, want 423", rec.Code)
	}
}

func TestLockEnforcement_LockedSourceOfMove(t *testing.T) {
	// RFC 4918 §7.1: a MOVE's source delete is a write — a locked source
	// without the token 423s even when the destination is free.
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "src.txt", []byte("data"))
	res := doLOCK(t, f, "/src.txt", lockBody, "", "")
	if res.status != 200 {
		t.Fatalf("LOCK: status = %d, want 200", res.status)
	}
	req := httptest.NewRequest("MOVE", "/src.txt", nil)
	req.Header.Set("Destination", "/free.txt")
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("MOVE of locked source: status = %d, want 423", rec.Code)
	}
	// COPY of a locked source (read-only on the source) still goes.
	if rec := serve(f, mustCopy("/src.txt", "/free.txt")); rec.Code != 201 {
		t.Fatalf("COPY of locked source: status = %d, want 201", rec.Code)
	}
}

func mustCopy(src, dst string) *http.Request {
	req := httptest.NewRequest("COPY", src, nil)
	req.Header.Set("Destination", dst)
	return req
}

func TestLockEnforcement_LockMethodsExempt(t *testing.T) {
	// Lock management never trips enforcement: LOCK creates/refreshes and
	// UNLOCK removes a lock on the very key the enforcement would 423.
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	res := doLOCK(t, f, "/a.txt", lockBody, "", "")
	if res.status != 200 {
		t.Fatalf("LOCK create: status = %d, want 200", res.status)
	}
	// Refresh (empty body + If token) → 200, exempt from the 423 gate.
	req := httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(""))
	req.Header.Set("If", "(<"+res.token+">)")
	req.Header.Set("Timeout", "Second-900")
	rec := serve(f, req)
	if rec.Code != 200 {
		t.Fatalf("LOCK refresh: status = %d, want 200; body=%.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Second-900") {
		t.Fatalf("refresh body missing Second-900:\n%s", rec.Body.String())
	}
	// UNLOCK is exempt too (correct-token path already covered): the
	// refresh lock releases cleanly.
	req = httptest.NewRequest("UNLOCK", "/a.txt", nil)
	req.Header.Set("Lock-Token", "<"+res.token+">")
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK: status = %d, want 204", rec.Code)
	}
}

func TestLockEnforcement_UnlockedWritesUnaffected(t *testing.T) {
	// Enforcement is a no-op on unlocked resources (never a spurious 423).
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "a.txt", []byte("x"))
	req := httptest.NewRequest("PUT", "/a.txt", strings.NewReader("y"))
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT unlocked: status = %d, want 204", rec.Code)
	}
	req = httptest.NewRequest("DELETE", "/a.txt", nil)
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE unlocked: status = %d, want 204", rec.Code)
	}
}

func TestLockExpiry_AllowsWrite(t *testing.T) {
	// Lazy expiry: a lock whose lease ran out no longer 423s writes (the
	// store removes it on access; 1-second lease, real clock).
	f, be := newLockingFrontend(t, Config{Bucket: "photos"})
	be.seed("photos", "a.txt", []byte("x"))
	req := httptest.NewRequest("LOCK", "/a.txt", strings.NewReader(lockBody))
	req.Header.Set("Timeout", "Second-1")
	rec := serve(f, req)
	if rec.Code != 200 {
		t.Fatalf("LOCK: status = %d, want 200", rec.Code)
	}
	<-time.After(2 * time.Second)
	req = httptest.NewRequest("PUT", "/a.txt", strings.NewReader("y"))
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT after expiry: status = %d, want 204; body=%.200s", rec.Code, rec.Body.String())
	}
}

func TestLOCK_PerBucketIsolation(t *testing.T) {
	// Locks are per bucket: locking /bkt1/a.txt does not 423 /bkt2/a.txt.
	f, _ := newLockingFrontend(t, Config{})
	be := f.be.(*stubBackend)
	be.seed("bkt1", "a.txt", []byte("1"))
	be.seed("bkt2", "a.txt", []byte("2"))
	if res := doLOCK(t, f, "/bkt1/a.txt", lockBody, "", ""); res.status != 200 {
		t.Fatalf("LOCK bkt1: status = %d, want 200", res.status)
	}
	req := httptest.NewRequest("PUT", "/bkt2/a.txt", strings.NewReader("y"))
	if rec := serve(f, req); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT bkt2 while bkt1 locked: status = %d, want 204", rec.Code)
	}
	req = httptest.NewRequest("PUT", "/bkt1/a.txt", strings.NewReader("y"))
	if rec := serve(f, req); rec.Code != http.StatusLocked {
		t.Fatalf("PUT bkt1: status = %d, want 423", rec.Code)
	}
}

func TestLock_OptionsAdvertiseMethods(t *testing.T) {
	f, _ := newLockingFrontend(t, Config{Bucket: "photos"})
	req := httptest.NewRequest("OPTIONS", "/", nil)
	rec := serve(f, req)
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "LOCK") || !strings.Contains(got, "UNLOCK") {
		t.Fatalf("Allow = %q, want LOCK and UNLOCK advertised", got)
	}
}
