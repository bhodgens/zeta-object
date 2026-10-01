package s3_test

// zmetad_wire_parity_test.go — Contract 5 wire-shape pin
// (zmetad-provider-2026-09 leaf 05, Task 1.3). The metadata package's
// parity gate (internal/metadata/parity_test.go,
// TestParityZmetadProviderBucket) proves core-S3 byte-identity and the
// provider-side 200/503 seam; THIS file drives the REAL zmetad provider
// over the e2e fixture database (scripts/e2e/fixtures/zmetad-fixture)
// through the REAL dispatch pipeline and pins the exact wire shape:
//
//   - JSON envelope key set EXACTLY {dataset, recordsLost, ringSwaps,
//     events}; per-event keys a subset of
//     {op, key, oldKey, txg, timestamp, sizeOld, sizeNew};
//   - canned fixture values on the wire: dataset testpool/e2e,
//     recordsLost=7 (knownLost only), ringSwaps=1 — never folded;
//   - XML ext: IsLossy=true, RecordsLost=7, RingSwaps=1.

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// buildZmetadFixture compiles + runs the e2e fixture builder mapping
// mountpoint, skipping (never failing) when go run cannot build - the
// case-18 graceful-skip convention.
func buildZmetadFixture(t *testing.T, mountpoint string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "zmetad-wire.db")
	cmd := exec.Command("go", "run", "./scripts/e2e/fixtures/zmetad-fixture", dbPath, mountpoint)
	cmd.Dir = filepath.Join("..", "..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("fixture builder cannot run on this host: %v\n%s", err, out)
	}
	return dbPath
}

// TestZmetadWireShapeContract5 pins the exact Contract 5 envelope on the
// wire with the canned fixture's loss values.
func TestZmetadWireShapeContract5(t *testing.T) {
	srv, root := newEventsTestServer(t)
	const bucket = "zmetadwire"
	if resp := doSigned(t, srv, "PUT", "/"+bucket, ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The provider EvalSymlinks before datasets-table resolution, so the
	// fixture maps the RESOLVED bucket root.
	bucketRoot, err := filepath.EvalSymlinks(filepath.Join(root, bucket))
	if err != nil {
		t.Fatalf("resolve bucket root: %v", err)
	}
	dbPath := buildZmetadFixture(t, bucketRoot)
	// Bypass ONLY the statfs hint (dev hosts have no ZFS); the datasets
	// + sync_state checks run against the real fixture DB.
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "1")

	// Production wiring shape (s3_wiring.go): a fresh provider per
	// resolve, availability decided by Probe.
	s3.InstallMetadataProvider(func(path string) metadata.MetadataProvider {
		p := metadata.NewZmetadEventsProvider(dbPath)
		res, err := p.Probe(t.Context(), path)
		if err != nil || !res.Available {
			return nil
		}
		return p
	})
	t.Cleanup(func() { s3.InstallMetadataProvider(nil) })

	// --- JSON envelope: exact Contract 5 key set ----------------------
	code, header, body := eventsGet(t, srv, "/"+bucket+"?events")
	if code != http.StatusOK {
		t.Fatalf("?events = %d, want 200 (body %s)", code, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q, want application/json", ct)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v (%s)", err, body)
	}
	wantKeys := map[string]bool{"dataset": true, "recordsLost": true, "ringSwaps": true, "events": true}
	if len(env) != len(wantKeys) {
		t.Fatalf("envelope key set = %v, want EXACTLY %v", keysOf(env), keysOf2(wantKeys))
	}
	for k := range env {
		if !wantKeys[k] {
			t.Fatalf("envelope has unexpected key %q (body %s)", k, body)
		}
	}
	if env["dataset"] != "testpool/e2e" {
		t.Fatalf("dataset = %v, want testpool/e2e", env["dataset"])
	}
	if env["recordsLost"] != float64(7) {
		t.Fatalf("recordsLost = %v, want 7 (knownLost ONLY - swaps never folded)", env["recordsLost"])
	}
	if env["ringSwaps"] != float64(1) {
		t.Fatalf("ringSwaps = %v, want 1", env["ringSwaps"])
	}

	// Per-event key discipline: every key must be one of the seven
	// Contract 5 fields; the canned rename row (full_path + old_full_path
	// resolved, no sizes) must carry EXACTLY op/key/oldKey/txg/timestamp.
	events, _ := env["events"].([]any)
	if len(events) != 11 {
		t.Fatalf("events = %d, want the 11 canned fixture rows", len(events))
	}
	allowed := map[string]bool{"op": true, "key": true, "oldKey": true, "txg": true,
		"timestamp": true, "sizeOld": true, "sizeNew": true}
	var sawRename bool
	for _, raw := range events {
		e, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("event is not an object: %v", raw)
		}
		for k := range e {
			if !allowed[k] {
				t.Fatalf("event carries non-Contract-5 key %q: %v", k, e)
			}
		}
		if e["op"] == "rename" && e["key"] == "edge/inner/renamed.txt" {
			sawRename = true
			for _, k := range []string{"op", "key", "oldKey", "txg", "timestamp"} {
				if _, ok := e[k]; !ok {
					t.Fatalf("rename event missing %q: %v", k, e)
				}
			}
			if _, ok := e["sizeOld"]; ok {
				t.Fatalf("rename event must not fabricate sizeOld: %v", e)
			}
			if e["oldKey"] != "edge/inner/deep.txt" {
				t.Fatalf("rename oldKey = %v, want the resolved old_full_path", e["oldKey"])
			}
			// NULL captured_at + zero hrtime -> honest zero time, never fabricated.
			if e["timestamp"] != "0001-01-01T00:00:00Z" {
				t.Fatalf("rename timestamp = %v, want zero-time RFC3339", e["timestamp"])
			}
		}
	}
	if !sawRename {
		t.Fatal("canned rename row missing from the wire")
	}

	// Key-scoped: the nested key resolves the PARTIAL create via the
	// conservative bare-name match (NULL full_path, ancestor absent).
	code, _, body = eventsGet(t, srv, "/"+bucket+"/edge/inner/orphan.txt?events")
	if code != http.StatusOK {
		t.Fatalf("scoped ?events = %d, want 200 (body %s)", code, body)
	}
	var scoped struct {
		Dataset     string `json:"dataset"`
		RecordsLost uint64 `json:"recordsLost"`
		RingSwaps   uint64 `json:"ringSwaps"`
		Events      []struct {
			Op  string `json:"op"`
			Key string `json:"key"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &scoped); err != nil {
		t.Fatalf("scoped envelope: %v (%s)", err, body)
	}
	if len(scoped.Events) != 1 || scoped.Events[0].Op != "create" || scoped.Events[0].Key != "orphan.txt" {
		t.Fatalf("scoped events = %+v, want exactly the PARTIAL orphan create", scoped.Events)
	}
	if scoped.RecordsLost != 7 || scoped.RingSwaps != 1 || scoped.Dataset != "testpool/e2e" {
		t.Fatalf("scoped envelope = %+v, want testpool/e2e 7 1", scoped)
	}

	// --- XML ext: IsLossy / RecordsLost / RingSwaps --------------------
	code, _, body = eventsGet(t, srv, "/"+bucket+"?events&versions")
	if code != http.StatusOK {
		t.Fatalf("?events&versions = %d, want 200 (body %s)", code, body)
	}
	var ext struct {
		XMLName     xml.Name `xml:"ListObjectVersionsExt"`
		Name        string   `xml:"Name"`
		IsLossy     bool     `xml:"IsLossy"`
		RecordsLost uint64   `xml:"RecordsLost"`
		RingSwaps   uint64   `xml:"RingSwaps"`
		Version     []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"Version"`
	}
	if err := xml.Unmarshal([]byte(body), &ext); err != nil {
		t.Fatalf("versions XML: %v (%s)", err, body)
	}
	if ext.XMLName.Local != "ListObjectVersionsExt" {
		t.Fatalf("XML root = %q, want ListObjectVersionsExt", ext.XMLName.Local)
	}
	if ext.Name != bucket {
		t.Fatalf("XML Name = %q, want the bucket name %q", ext.Name, bucket)
	}
	if !ext.IsLossy {
		t.Fatal("IsLossy must be true (RecordsLost=7 > 0, Contract 5)")
	}
	if ext.RecordsLost != 7 {
		t.Fatalf("XML RecordsLost = %d, want 7 (never folded with swaps)", ext.RecordsLost)
	}
	if ext.RingSwaps != 1 {
		t.Fatalf("XML RingSwaps = %d, want 1", ext.RingSwaps)
	}
	if len(ext.Version) == 0 {
		t.Fatal("versions XML derived no entries from the canned create/rename/truncate/remove rows")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOf2(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
