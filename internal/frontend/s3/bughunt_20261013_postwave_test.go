// bughunt_20261013_postwave_test.go — regression pins for the 2026-10-13
// post-fix-wave s3 findings:
//
//   - F2: CompleteMultipartUpload's finalize ran the tags restore BEFORE
//     recordCapturedObjectVersion, so a tagStore.Put failure 500'd with
//     the version history not yet restored under the wholesale-rewritten
//     sidecar — the captured old version was dropped forever. Fixed by
//     reordering: version record FIRST, tags restore SECOND (a tag-restore
//     failure then leaves data + history committed and only tags lost).
package s3

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// ---------- F2: version record precedes the tags restore ----------

func TestBughuntF2_TagRestoreFailureKeepsVersionRecord(t *testing.T) {
	env := newVerEnv(t, "f2ver-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}

	// v1: plain PUT creates the object WITH tags (a create — records no
	// version, but the tags must survive the coming completion).
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/doc.txt", strings.NewReader("v1-bytes"))
	req.Header.Set("x-amz-tagging", "env=prod&team=core")
	w := httptest.NewRecorder()
	putObjectHandler(w, req, env.bucket, "doc.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("put v1: %d %s", w.Code, w.Body.String())
	}

	// Force the completion-time tagStore.Put to fail through the runner
	// seam: the completion's tagsForCompletion Get must still succeed (a
	// failure there would 500 BEFORE the rename and prove nothing), so the
	// script fails only the --tag-set exec.
	argvs := scriptZmetadTagRunnerFailingPut(t, env.dataDir)

	uploadID := mpSeedUploadWithParts(t, env.bucketPath, "doc.txt", map[int]string{1: "v2-part"})
	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("v2-part")})
	w = httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost,
		"/"+env.bucket+"/doc.txt?uploadId="+uploadID, body), env.bucket, "doc.txt", uploadID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("complete = %d %s, want 500 (the forced tag-restore failure)", w.Code, w.Body.String())
	}

	// The restore attempted and failed — the completion got as far as the
	// (now reordered) post-rename bookkeeping.
	if len(*argvs) == 0 {
		t.Fatal("no tag exec observed: the completion never reached the tags restore (test seam broken)")
	}

	// F2 core assertion: the captured v1 SURVIVES — the version record ran
	// BEFORE the failing tags restore, so data + history stay committed.
	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List after failed-restore completion: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d entries, want 1 (F2: a tag-restore failure must not drop the recorded version)", len(versions))
	}
	rc, entry, err := store.Open(env.bucket, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	if string(raw) != "v1-bytes" {
		t.Errorf("recorded version bytes = %q, want v1-bytes", raw)
	}
	if entry.Size != int64(len("v1-bytes")) {
		t.Errorf("recorded version size = %d, want %d", entry.Size, len("v1-bytes"))
	}

	// Data is committed (the rename preceded both bookkeeping steps).
	if w := verGet(t, env, "doc.txt"); w.Body.String() != "v2-part" {
		t.Errorf("current bytes = %q, want v2-part", w.Body.String())
	}
}

func TestBughuntF2_HappyPathUnchanged(t *testing.T) {
	env := newVerEnv(t, "f2happy-bucket")
	if w := verEnable(t, env, "Enabled"); w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if w := verPutBody(t, env, "doc.txt", "v1-bytes"); w.Code != http.StatusOK {
		t.Fatalf("put v1: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPut, "/"+env.bucket+"/doc.txt", strings.NewReader("x"))
	req.Header.Set("x-amz-tagging", "env=prod")
	w := httptest.NewRecorder()
	putObjectHandler(w, req, env.bucket, "doc.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("tag v1: %d %s", w.Code, w.Body.String())
	}

	uploadID := mpSeedUploadWithParts(t, env.bucketPath, "doc.txt", map[int]string{1: "v2-part"})
	body := mpCompleteBody(PartToUpload{PartNumber: 1, ETag: mpHashETag("v2-part")})
	w = httptest.NewRecorder()
	completeMultipartUploadHandler(w, httptest.NewRequest(http.MethodPost,
		"/"+env.bucket+"/doc.txt?uploadId="+uploadID, body), env.bucket, "doc.txt", uploadID)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}

	store := versionStoreForBucket(env.bucketPath)
	versions, err := store.List(env.bucket, "doc.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Two recorded versions: the tagged PUT recorded v1, then the
	// completion recorded the "x" overwrite beneath which the captured
	// v1 history was restored. The reordered tags restore must have kept
	// BOTH (the old lossy sidecar write-back dropped the second one).
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2 (the reordered restore must preserve the recorded history)", len(versions))
	}
	// The gate/runner seams installed for the failing-Put test are gone
	// here; the sidecar store answers. The restore wrote the captured
	// prior tags into the rewritten sidecar.
	tags, err := sidecarTagStore{bucketPath: env.bucketPath}.Get("doc.txt")
	if err != nil {
		t.Fatalf("Get tags: %v", err)
	}
	if len(tags) != 1 || tags["env"] != "prod" {
		t.Fatalf("tags after complete = %v, want env=prod (the reordered restore still runs on success)", tags)
	}
	if w := verGet(t, env, "doc.txt"); w.Body.String() != "v2-part" {
		t.Errorf("current bytes = %q, want v2-part", w.Body.String())
	}
}

// scriptZmetadTagRunnerFailingPut installs the zfs_native_tags gate in
// zmetad mode with a fake dataset probe (every bucket resolves), a fake
// zmetad DB whose event log resolves the completion key to an object id,
// and a scripted runner whose --tag-get succeeds and whose --tag-set
// FAILS — forcing exactly the finalizeComplete tags-restore failure.
func scriptZmetadTagRunnerFailingPut(t *testing.T, dataDir string) *[][]string {
	t.Helper()

	// The exists probe: every dataset "exists" (zmetad-managed bucket).
	if err := InstallZmetadTagStore("testpool/f2", "zfs", "zmetad", writeF2ZmetadDB(t), dataDir+"/", func(context.Context, string) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("InstallZmetadTagStore: %v", err)
	}
	t.Cleanup(UninstallZmetadTagStore)

	argvs := &[][]string{}
	orig := zmetadTagRunner
	zmetadTagRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
		*argvs = append(*argvs, append([]string{binary}, args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "--tag-get"):
			return []byte("env=prod\nteam=core\n"), "", nil
		case strings.Contains(joined, "--tag-set"):
			return []byte(""), "database is locked", errors.New("exit status 1")
		default: // --tag-clear or anything else: succeed
			return []byte(""), "", nil
		}
	}
	t.Cleanup(func() { zmetadTagRunner = orig })
	return argvs
}

// writeF2ZmetadDB builds a minimal zmetad-v5-schema DB (the same DDL
// internal/metadata's tests pin) with one sync_state row and one events
// row whose full_path resolves the test key, so defaultObjectIDResolver
// succeeds and the store reaches the scripted runner.
func writeF2ZmetadDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zmetad.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fake zmetad db: %v", err)
	}
	defer conn.Close()
	const ddl = `
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    dataset TEXT NOT NULL,
    txg INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    object_id INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    full_path TEXT,
    old_full_path TEXT,
    UNIQUE(dataset, txg, object_id, event_type, timestamp)
);
CREATE TABLE IF NOT EXISTS sync_state (
    dataset TEXT PRIMARY KEY,
    last_offset INTEGER NOT NULL,
    last_sync INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`
	if _, err := conn.Exec(ddl); err != nil {
		t.Fatalf("create fake schema: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO meta VALUES ('db_schema_version', '5')`,
		`INSERT INTO meta VALUES ('events_schema_version', '2')`,
		`INSERT INTO sync_state VALUES ('testpool/f2/f2ver-bucket', 0, 0)`,
		`INSERT INTO events (dataset, txg, timestamp, object_id, event_type, full_path)
		 VALUES ('testpool/f2/f2ver-bucket', 1, 1, 42, 'write', 'doc.txt')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("seed fake zmetad db (%s): %v", q, err)
		}
	}
	return path
}
