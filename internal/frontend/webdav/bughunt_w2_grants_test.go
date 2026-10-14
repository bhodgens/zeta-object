// bughunt_w2_grants_test.go — regression pins for the wave-2 webdav
// grant-vocabulary findings:
//
//   - W2-M3: the webdav grant gate drifted from the s3 op vocabulary —
//     LOCK/UNLOCK fell to the read class (a read-only principal could lock
//     every key it can read and 423 every writer off the object), and
//     DELETE required only write (a write-only webdav identity could
//     delete; a delete-only s3 grant 403'd on webdav). LOCK/UNLOCK now
//     require write; DELETE additionally requires the delete grant through
//     the same auth.AuthorizeOp the s3 frontend uses.
//   - W2-M1 (webdav half): POST ?batch requires write AND delete — the
//     manifest may delete and the executor runs no per-item grant check.
package webdav

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// webdavLockBody is the RFC 4918 §9.10.6 LOCK request body (a copy of
// lockhandler_test.go's — this file stands alone from that suite).
const webdavLockBody = `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner><D:href>mailto:alice@example.com</D:href></D:owner></D:lockinfo>`

// w2GrantRegistry builds a registry with ONE identity whose grant on the
// bucket is exactly the given op list.
func w2GrantRegistry(t *testing.T, bucket, ops string) *auth.BasicAuthenticator {
	t.Helper()
	expr, err := auth.ParseGrantValue("probe", bucket, json.RawMessage(`{"ops":[`+ops+`]}`))
	if err != nil {
		t.Fatalf("grant fixture (%s): %v", ops, err)
	}
	opsList := make([]string, 0, len(expr.Ops))
	for _, op := range []auth.Op{"read", "write", "list", "delete", "create"} {
		if expr.Ops[op] {
			opsList = append(opsList, string(op))
		}
	}
	rawJSON, err := json.Marshal(struct {
		Ops []string `json:"ops"`
	}{Ops: opsList})
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{
		{Name: "probe", AccessKey: "AKW2M3", SecretKey: "sk-w2m3",
			Grants: map[string]json.RawMessage{bucket: rawJSON}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewBasicAuthenticator(reg)
}

// w2Harness is a mode-B frontend over the real fs backend with a single
// authenticated identity.
type w2Harness struct {
	t          *testing.T
	f          *Frontend
	bucket     string
	bucketPath string
}

func newW2Harness(t *testing.T, bucket, ops string) *w2Harness {
	t.Helper()
	dataDir := t.TempDir()
	s3.SetupVersioningTestEnv(t, dataDir)
	bucketPath := filepath.Join(dataDir, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("creating bucket dir: %v", err)
	}
	f, err := New(s3.TestBackend(), Config{Bucket: bucket},
		WithAuthenticator(w2GrantRegistry(t, bucket, ops)),
		WithBucketPathResolver(func(b string) string { return filepath.Join(dataDir, b) }),
		WithLockStoreRoot(func(string) string { return bucketPath }))
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	return &w2Harness{t: t, f: f, bucket: bucket, bucketPath: bucketPath}
}

// seed writes an object through the real fs backend.
func (h *w2Harness) seed(key, body string) {
	h.t.Helper()
	be := s3.TestBackend()
	if _, err := be.Put(h.t.Context(), h.bucket, key, strings.NewReader(body), int64(len(body)), objectmodel.PutOptions{}); err != nil {
		h.t.Fatalf("seed %s: %v", key, err)
	}
}

// do issues a Basic-authed request; returns the status.
func (h *w2Harness) do(method, path, body string) int {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if method == "PROPFIND" {
		req.Header.Set("Depth", "1")
	}
	req.SetBasicAuth("AKW2M3", "sk-w2m3")
	rec := httptest.NewRecorder()
	h.f.Handler().ServeHTTP(rec, req)
	_, _ = io.Copy(io.Discard, rec.Body)
	return rec.Code
}

// TestW2M3_WebdavGrantVocabularyMatchesS3 pins the webdav grant matrix:
// delete-only DELETE 2xx, write-only DELETE 403, read-only LOCK 403,
// write identity LOCK 200.
func TestW2M3_WebdavGrantVocabularyMatchesS3(t *testing.T) {
	const bucket = "w2m3-photos"

	t.Run("delete-only identity DELETEs", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"delete"`)
		h.seed("a.txt", "data")
		if code := h.do("DELETE", "/a.txt", ""); code >= 400 {
			t.Fatalf("delete-only identity DELETE = %d, want 2xx (W2-M3: the s3 DELETE grant must work on webdav)", code)
		}
	})

	t.Run("write-only identity DELETE is 403", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"write"`)
		h.seed("a.txt", "data")
		if code := h.do("DELETE", "/a.txt", ""); code != http.StatusForbidden {
			t.Fatalf("write-only identity DELETE = %d, want 403 (W2-M3: delete needs the delete grant)", code)
		}
	})

	t.Run("read-only identity LOCK is 403", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"read","list"`)
		h.seed("a.txt", "data")
		if code := h.do("LOCK", "/a.txt", webdavLockBody); code != http.StatusForbidden {
			t.Fatalf("read-only identity LOCK = %d, want 403 (W2-M3: locking mutates lock state)", code)
		}
	})

	t.Run("write identity LOCK is 200", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"read","write"`)
		h.seed("a.txt", "data")
		if code := h.do("LOCK", "/a.txt", webdavLockBody); code != http.StatusOK {
			t.Fatalf("write identity LOCK = %d, want 200", code)
		}
	})
}

// TestW2M1_WebdavBatchRequiresWriteAndDelete pins the webdav ?batch mount
// gate (the s3-mount parity half of W2-M1).
func TestW2M1_WebdavBatchRequiresWriteAndDelete(t *testing.T) {
	const bucket = "w2m1-dav-bucket"
	batchBody := `{"operations":[{"op":"delete","from":"victim.txt"}]}`

	t.Run("write-only identity batch is 403", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"write"`)
		if code := h.do("POST", "/?batch", batchBody); code != http.StatusForbidden {
			t.Fatalf("webdav POST ?batch as write-only = %d, want 403 (W2-M1: a manifest can delete)", code)
		}
	})

	t.Run("read-only identity batch is 403", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"read","list"`)
		if code := h.do("POST", "/?batch", batchBody); code != http.StatusForbidden {
			t.Fatalf("webdav POST ?batch as read-only = %d, want 403", code)
		}
	})

	t.Run("write+delete identity batch executes", func(t *testing.T) {
		h := newW2Harness(t, bucket, `"read","write","list","delete","create"`)
		if code := h.do("POST", "/?batch", batchBody); code == http.StatusForbidden {
			t.Fatalf("webdav POST ?batch with write+delete = 403: over-tightened (W2-M1)")
		}
	})
}

// TestBatchLockBypass_ManifestDeleteOnLockedKeyIs423 pins the wave-2 LOW
// finding (dispatch.go:58 vs :84-98): batchDispatch resolves BEFORE the
// method switch, so DELETE's enforceWriteLock never ran for a manifest
// delete — a live WebDAV lock was bypassed by POST ?batch. Pre-fix this
// test's 423 assertion FAILS (the batch executes the delete and answers
// 200); post-fix the whole manifest is refused 423 before any item runs,
// and the lock survives (the positive-control batch after UNLOCK shows the
// same manifest was executable).
func TestBatchLockBypass_ManifestDeleteOnLockedKeyIs423(t *testing.T) {
	const bucket = "w2lock-bucket"
	// The harness identity holds write+delete so the W2-M1 grant gate is
	// not what answers here — only the lock gate is under test.
	h := newW2Harness(t, bucket, `"read","write","list","delete","create"`)
	h.seed("locked-victim.txt", "do-not-delete")

	// LOCK the key as the same identity (it holds write; W2-M3 pins that).
	if code := h.do("LOCK", "/locked-victim.txt", webdavLockBody); code != http.StatusOK {
		t.Fatalf("LOCK setup = %d, want 200", code)
	}

	// The bypass: a manifest delete on the locked key. Pre-fix: 200 + the
	// object gone. Post-fix: 423, nothing executed.
	code := h.do("POST", "/?batch", `{"operations":[{"op":"delete","from":"locked-victim.txt"}]}`)
	if code != http.StatusLocked {
		t.Fatalf("POST ?batch delete on a LOCKED key = %d, want 423 (the ?batch sub-resource must not bypass a live WebDAV lock)", code)
	}

	// The lock is intact AND the victim survived: the holder's own
	// single-op DELETE now answers 423 (the lock was not consumed), and a
	// plain GET still finds the object.
	if code := h.do("DELETE", "/locked-victim.txt", ""); code != http.StatusLocked {
		t.Fatalf("single-op DELETE on the locked key = %d, want 423 (lock must survive the batch refusal)", code)
	}
	if code := h.do("GET", "/locked-victim.txt", ""); code != http.StatusOK {
		t.Fatalf("GET after the refused batch = %d, want 200 (nothing executed)", code)
	}

	// Positive control: after UNLOCK the same manifest deletes — the 423
	// was the lock gate, not a broken batch surface. UNLOCK takes the
	// Lock-Token header (RFC 4918 §9.11), the same shape lockhandler_test
	// pins.
	lockTok := h.lockToken("locked-victim.txt")
	if lockTok == "" {
		t.Fatalf("could not resolve the lock token for the positive control")
	}
	if code := h.doWithIf("UNLOCK", "/locked-victim.txt", "Lock-Token", "<"+lockTok+">", ""); code != http.StatusNoContent {
		t.Fatalf("UNLOCK setup = %d, want 204", code)
	}
	if code := h.do("POST", "/?batch", `{"operations":[{"op":"delete","from":"locked-victim.txt"}]}`); code >= 400 {
		t.Fatalf("POST ?batch delete after UNLOCK = %d, want 2xx (positive control)", code)
	}
	if code := h.do("GET", "/locked-victim.txt", ""); code != http.StatusNotFound {
		t.Fatalf("GET after the unlocked batch = %d, want 404 (the batch really executed)", code)
	}
}

// lockToken reads the live lock's token out of the file lockStore. The
// store is keyed by lockKey's shape ("/<bucket>/<key>"); the caller passes
// the bare object key.
func (h *w2Harness) lockToken(key string) string {
	store := h.f.lockStoreFor(h.bucket)
	info, err := store.Get("/" + h.bucket + "/" + key)
	if err != nil {
		return ""
	}
	return info.Token
}

// doWithIf is do with one extra header (used for the UNLOCK positive
// control, which must carry the lock token).
func (h *w2Harness) doWithIf(method, path, name, value, body string) int {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set(name, value)
	req.SetBasicAuth("AKW2M3", "sk-w2m3")
	rec := httptest.NewRecorder()
	h.f.Handler().ServeHTTP(rec, req)
	_, _ = io.Copy(io.Discard, rec.Body)
	return rec.Code
}
