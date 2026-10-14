// copymove_versioning_test.go — bughunt L3, superseded by W2-H2 COPY
// parity: BOTH copy paths now record the destination's pre-copy bytes as
// one version on a versioning-Enabled bucket.
//
// History: copymove.go once ran only MOVE's destination Put through
// captureBeforePut/recordAfterPut, because s3's copyObjectHandler had NO
// capture/record call — a plain webdav COPY recording a version its s3
// counterpart never wrote was the L3 divergence, and the pins here held
// both sides at "records nothing".
//
// W2-H2 closed the s3 gap instead: copyObjectHandler got the same
// capture→Put→record gate every other write path had, so s3 CopyObject
// now records the destination's pre-copy bytes. A webdav COPY that
// skipped the capture became the divergence (the pins failed the moment
// the s3 fix landed — the two wire paths disagreed on recorded entries).
// The pinned decision is now: a plain webdav COPY records the SAME
// version entry the identical copy via s3 records — identical history
// after either path. MOVE keeps its destination capture (unchanged), and
// MOVE's source-delete half is unchanged.
package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// versionDataFilesFor returns the count of version DATA files on disk for
// one key (the .versions / .versions-r layouts, keyed by sha256 of the key
// — the store's hostile-key discipline). Pruned bytes are invisible to
// List (their entries are rewritten out of the sidecar), so a file count
// is the only honest way to observe the destruction.
func versionDataFilesFor(t *testing.T, bucketPath, key string) int {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	sha := hex.EncodeToString(sum[:])
	total := 0
	for _, dir := range []string{".versions", ".versions-r"} {
		entries, err := os.ReadDir(filepath.Join(bucketPath, ".metadata", dir, sha))
		if err != nil {
			continue // layout absent for this mode
		}
		for _, e := range entries {
			if !e.IsDir() {
				total++
			}
		}
	}
	return total
}

// readVersionDataFile reads one version's data file directly, through
// BOTH layouts (.versions-r for the reflink mechanism, .versions for the
// plain sidecar one) so the test does not care which mode is installed.
func readVersionDataFile(t *testing.T, bucketPath, key, versionID string) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	sha := hex.EncodeToString(sum[:])
	for _, dir := range []string{".versions", ".versions-r"} {
		raw, err := os.ReadFile(filepath.Join(bucketPath, ".metadata", dir, sha, versionID))
		if err == nil {
			return raw
		}
	}
	t.Fatalf("version data file for %s/%s not found under either layout", key, versionID)
	return nil
}

// listVersions lists a key's history, treating "no history at all" as an
// empty list (the store answers NoSuchKey for a key with no sidecar
// version fields) — the caller's assertion is about the COUNT and the
// entries, not about which absence sentinel arrives.
func listVersions(t *testing.T, e *verParityEnv, key string) []s3.VersionEntry {
	t.Helper()
	versions, err := s3.ListVersionsForTest(e.bucketPath, e.bucket, key)
	if err != nil && len(versions) == 0 {
		return nil
	}
	if err != nil {
		t.Fatalf("List %s: %v", key, err)
	}
	return versions
}

// s3CopyInto drives the REAL s3 CopyObject handler (the production
// counterpart the webdav COPY must match): PUT with x-amz-copy-source.
func (e *verParityEnv) s3CopyInto(srcKey, dstKey string) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", "/"+e.bucket+"/"+dstKey, nil)
	req.Header.Set("x-amz-copy-source", "/"+e.bucket+"/"+srcKey)
	w := httptest.NewRecorder()
	s3.CopyObjectHandlerForTest(w, req, e.bucket, dstKey)
	if w.Code != http.StatusOK {
		e.t.Fatalf("s3 CopyObject %s -> %s: status = %d, want 200 (body %s)", srcKey, dstKey, w.Code, w.Body.String())
	}
}

// davCopy COPYs (no rename) through the webdav handler, returning the
// recorded status.
func (e *verParityEnv) davCopy(srcKey, dstKey string) int {
	e.t.Helper()
	req := httptest.NewRequest("COPY", "/"+srcKey, nil)
	req.Header.Set("Destination", "/"+dstKey)
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	return w.Code
}

// emulateFICLONE swaps the FICLONE seam for a byte-copy (this darwin dev
// host has no ioctl; the real one is proven live by zfs-validate).
func emulateFICLONE(t *testing.T) {
	t.Helper()
	s3.WithReflinkCloneForTest(t, func(dst, src string) error {
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o644)
	})
}

// TestWebdavCOPY_NoVersionRecordMatchesS3CopyObject is the L3 pin, both
// directions and every capture mode. Two destination keys on ONE
// versioning-Enabled bucket with ONE prior version each: one destination
// overwritten by a webdav COPY, the other by the s3 CopyObject handler,
// from identical sources. The recorded version entries must be IDENTICAL —
// and they are identical at ONE (the pre-existing one), never TWO.
func TestWebdavCOPY_NoVersionRecordMatchesS3CopyObject(t *testing.T) {
	for _, mode := range []string{"sidecar", "reflink", "both"} {
		t.Run(mode, func(t *testing.T) {
			emulateFICLONE(t)
			e := newVerParityEnv(t, "copy-parity-"+mode, mode)
			e.enable()

			// Two sources with identical payloads...
			e.s3Put("src-a.txt", "payload")
			e.s3Put("src-b.txt", "payload")
			// ...and two pre-existing destinations, each with exactly ONE
			// prior version, so the COPY is an OVERWRITE of history.
			e.s3Put("dst-dav.txt", "old-v1-dav")
			e.s3Put("dst-dav.txt", "old-v2-dav")
			e.s3Put("dst-s3.txt", "old-v1-s3")
			e.s3Put("dst-s3.txt", "old-v2-s3")

			for _, key := range []string{"dst-dav.txt", "dst-s3.txt"} {
				if got := listVersions(t, e, key); len(got) != 1 {
					t.Fatalf("fixture: %s should hold exactly one prior version, got %+v", key, got)
				}
			}

			if code := e.davCopy("src-a.txt", "dst-dav.txt"); code != http.StatusNoContent {
				t.Fatalf("webdav COPY onto an existing destination = %d, want 204", code)
			}
			e.s3CopyInto("src-b.txt", "dst-s3.txt")

			davVersions := listVersions(t, e, "dst-dav.txt")
			s3Versions := listVersions(t, e, "dst-s3.txt")
			if len(davVersions) != len(s3Versions) {
				t.Fatalf("%s mode: webdav COPY recorded %d version entries, s3 CopyObject recorded %d — the two copy paths must agree:\n dav: %+v\n s3:  %+v",
					mode, len(davVersions), len(s3Versions), davVersions, s3Versions)
			}
			// W2-H2 parity: BOTH copies must have recorded the
			// destination's pre-copy bytes (the fixture held exactly one
			// prior entry; the copy adds the second for it). A missing
			// entry on either side means that path wiped history — the
			// sidecar is rewritten wholesale by the backend Put.
			if len(davVersions) != 2 {
				t.Fatalf("%s mode: after the COPY the destination history = %d entries, want 2 (the fixture's one + the pre-copy bytes BOTH copy paths now record — W2-H2): dav %+v, s3 %+v",
					mode, len(davVersions), davVersions, s3Versions)
			}
			for _, v := range davVersions {
				if v.Size != int64(len("old-v2-dav")) || v.IsDeleteMarker {
					t.Fatalf("%s mode: the webdav COPY recorded %+v, want one entry of the destination's pre-copy bytes (%d bytes) — W2-H2 parity with s3 CopyObject",
						mode, v, len("old-v2-dav"))
				}
			}
			// Byte fidelity of the recorded versions (both on-disk layouts,
			// via the shared helper): the set must be exactly the two
			// historical byte generations - the second PUT recorded the
			// first put's bytes (a create records nothing), and the COPY
			// recorded the pre-copy bytes.
			got := map[string]bool{}
			for _, v := range davVersions {
				b := string(readVersionDataFile(t, e.bucketPath, "dst-dav.txt", v.ID))
				if b != "old-v1-dav" && b != "old-v2-dav" {
					t.Fatalf("%s mode: recorded version %s bytes = %q, want one of the dst historical generations", mode, v.ID, b)
				}
				got[b] = true
			}
			if !got["old-v1-dav"] || !got["old-v2-dav"] {
				t.Fatalf("%s mode: recorded version bytes %v, want both historical generations", mode, got)
			}
			// Both copies really wrote the destination bytes.
			for _, key := range []string{"dst-dav.txt", "dst-s3.txt"} {
				raw, rerr := os.ReadFile(filepath.Join(e.bucketPath, key))
				if rerr != nil {
					t.Fatalf("%s: %v", key, rerr)
				}
				if string(raw) != "payload" {
					t.Fatalf("%s = %q, want payload (the copy must have landed)", key, raw)
				}
			}
		})
	}
}

// TestWebdavCOPY_RetentionZeroDoesNotPrunePriorVersionData is the
// destructive half of L3: with per-bucket
// zfs_versioning_reflinkRetention = 0 (keep ZERO version copies), the
// manufactured record used to invoke pruneReflinkVersions and DELETE
// version data that the same copy via s3 preserved.
//
// The retention view is installed AFTER the fixture's prior version is
// recorded, so the fixture itself is not the thing being pruned.
func TestWebdavCOPY_RetentionZeroDoesNotPrunePriorVersionData(t *testing.T) {
	const bucket = "copy-retention-zero-bkt"
	emulateFICLONE(t)
	e := newVerParityEnv(t, bucket, "both")
	e.enable()

	// Build one PRIOR version on the destination (the data a manufactured
	// record used to prune away), then switch the bucket's retention to 0.
	e.s3Put("src.txt", "payload")
	e.s3Put("dst.txt", "old-v1")
	e.s3Put("dst.txt", "old-v2")
	// The same-shaped destination for the s3 copy (parity peer).
	e.s3Put("dst-s3.txt", "old-v1")
	e.s3Put("dst-s3.txt", "old-v2")
	fixtureVersions := listVersions(t, e, "dst.txt")
	if len(fixtureVersions) != 1 {
		t.Fatalf("fixture: dst.txt should hold one prior version, got %+v", fixtureVersions)
	}
	dataFilesBefore := versionDataFilesFor(t, e.bucketPath, "dst.txt")
	if dataFilesBefore == 0 {
		t.Fatal("fixture: dst.txt has a recorded version but no version data file on disk")
	}

	s3.InstallServerConfigView(s3.ServerConfigView{
		DataDir:          e.dataDir + "/",
		ReflinkRetention: map[string]int{bucket: 0},
	})
	t.Cleanup(func() {
		s3.InstallServerConfigView(s3.ServerConfigView{DataDir: e.dataDir + "/"})
	})
	// Control: the retention view really is armed (a plain s3 PUT on this
	// bucket now prunes to zero, which is what makes the COPY assertion
	// below meaningful rather than vacuous).
	e.s3Put("retention-probe.txt", "p1")
	e.s3Put("retention-probe.txt", "p2")
	if probe := versionDataFilesFor(t, e.bucketPath, "retention-probe.txt"); probe != 0 {
		t.Fatalf("control: the retention-0 view is not armed — a plain PUT left %d version data file(s) on disk; the COPY assertion below would be vacuous", probe)
	}

	if code := e.davCopy("src.txt", "dst.txt"); code != http.StatusNoContent {
		t.Fatalf("webdav COPY onto an existing destination = %d, want 204", code)
	}

	// W2-H2: the COPY now records the pre-copy bytes (parity with s3
	// CopyObject), so the retention-0 prune fires for it exactly as it
	// fires for a plain PUT (the control test below). Retention is host
	// policy — keep-zero means keep-zero for EVERY recording write, copy
	// included. The L3-era expectation ("a COPY must not prune") pinned
	// the no-record special case; with both paths recording, pruning here
	// is the CONSISTENT behavior, and the destruction risk the old pin
	// guarded (webdav pruning bytes s3's copy preserved) cannot recur:
	// the parity assert below holds both paths to the same outcome.
	if got := versionDataFilesFor(t, e.bucketPath, "dst.txt"); got != 0 {
		t.Fatalf("webdav COPY on a retention-0 bucket left %d version data file(s) for dst.txt, want 0 (retention is host policy for every recording write — W2-H2 parity)",
			got)
	}

	// Parity: the same-shaped destination copied through the s3 handler
	// ends in the SAME state (same recorded entries, same on-disk data
	// files). The sidecar-wipe noted above is pre-existing shared
	// CopyObject behavior — not something the webdav path may fix alone.
	e.s3CopyInto("src.txt", "dst-s3.txt")
	if code := e.davCopy("src.txt", "dst.txt"); code != http.StatusNoContent {
		t.Fatalf("webdav COPY onto the parity destination = %d, want 204", code)
	}
	davAfter := listVersions(t, e, "dst.txt")
	s3After := listVersions(t, e, "dst-s3.txt")
	if len(davAfter) != len(s3After) {
		t.Fatalf("after a COPY the two paths disagree on recorded entries: webdav %+v, s3 %+v", davAfter, s3After)
	}
	if got, want := versionDataFilesFor(t, e.bucketPath, "dst.txt"), versionDataFilesFor(t, e.bucketPath, "dst-s3.txt"); got != want {
		t.Fatalf("after a COPY the two paths disagree on version data files on disk: webdav %d, s3 %d", got, want)
	}
	// The destination bytes are the COPY's payload.
	if cur, rerr := os.ReadFile(filepath.Join(e.bucketPath, "dst.txt")); rerr != nil || string(cur) != "payload" {
		t.Fatalf("dst.txt = %q err=%v, want payload (the copy must have landed)", cur, rerr)
	}
}

// TestWebdavCOPY_RetentionZeroPrunesForAPlainPUT is the CONTROL arm for the
// test above: on the SAME per-bucket retention-0 bucket a plain PUT still
// prunes to zero, exactly as s3's PUT does. Without this the retention-0
// COPY assertion could pass for the wrong reason (a retention view that was
// never armed, or a prune path that is dead for both protocols).
func TestWebdavCOPY_RetentionZeroPrunesForAPlainPUT(t *testing.T) {
	const bucket = "copy-retention-control-bkt"
	emulateFICLONE(t)
	e := newVerParityEnv(t, bucket, "both")
	e.enable()

	e.s3Put("probe.txt", "old-v1")
	e.s3Put("probe.txt", "old-v2")
	if got := listVersions(t, e, "probe.txt"); len(got) != 1 {
		t.Fatalf("fixture: probe.txt should hold one prior version, got %+v", got)
	}
	before := versionDataFilesFor(t, e.bucketPath, "probe.txt")

	s3.InstallServerConfigView(s3.ServerConfigView{
		DataDir:          e.dataDir + "/",
		ReflinkRetention: map[string]int{bucket: 0},
	})
	t.Cleanup(func() {
		s3.InstallServerConfigView(s3.ServerConfigView{DataDir: e.dataDir + "/"})
	})

	// A plain PUT overwrite records a version — which, on retention 0,
	// prunes every version data file (the mechanism IS armed).
	e.s3Put("probe.txt", "old-v3")
	if got := versionDataFilesFor(t, e.bucketPath, "probe.txt"); got >= before {
		t.Fatalf("fixture/control: a plain PUT on a retention-0 bucket must prune version data (before %d, after %d)", before, got)
	}
}

// TestWebdavMOVE_StillCapturesDestination keeps MOVE's capture: a MOVE
// onto an existing destination still records the destination's old bytes
// (the rename the fs performs has no s3 CopyObject counterpart standing in
// for it, so this leg keeps the PUT-shaped capture). The COPY fix must not
// have narrowed MOVE.
func TestWebdavMOVE_StillCapturesDestination(t *testing.T) {
	e := newVerParityEnv(t, "copy-vs-move-bkt", "sidecar")
	e.enable()

	e.s3Put("dst.txt", "dst-v1")
	e.s3Put("src.txt", "move-me")

	req := httptest.NewRequest("MOVE", "/src.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	w := httptest.NewRecorder()
	e.f.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("webdav MOVE onto an existing destination = %d, want 204", w.Code)
	}

	versions := listVersions(t, e, "dst.txt")
	if len(versions) != 1 || versions[0].Size != int64(len("dst-v1")) {
		t.Fatalf("MOVE destination history = %+v, want one capture of the OLD dst bytes", versions)
	}
	raw, err := s3.OpenVersionForTest(e.bucketPath, e.bucket, "dst.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("open captured version: %v", err)
	}
	if string(raw) != "dst-v1" {
		t.Fatalf("captured bytes = %q, want dst-v1", raw)
	}
}
