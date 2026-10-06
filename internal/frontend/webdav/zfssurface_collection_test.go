// zfssurface_collection_test.go — bughunt L2: ?events on a NESTED
// COLLECTION path routed to the key-scoped surface.
//
// zfssurface.go picked the surface with `res.key == ""`, but parseResource
// gives a nested collection a NON-EMPTY key ("photos/report/" parses to
// key "photos/report", isCollection=true). The key-scoped surface's
// provider History call then went through rowsMatchKey's PARTIAL-row rule
// (bareMatchesKey: bare-name equality OR the queried key ending in
// "/"+bare), so GET /photos/report/?events could answer with the events of
// a completely UNRELATED object — a zmetad PARTIAL row whose stored path
// is the bare name "report" matches the queried key "photos/report" by the
// suffix rule.
//
// S3 has no collection concept, so this class of wrong answer is one the
// s3 surface cannot produce — the webdav surface must not invent it.
//
// PINNED DECISION: a ?events GET on a path that is NOT an object answers
// an EMPTY event history with the honest envelope (200,
// application/json, events: []). Not 404 — the bucket exists, the query is
// known, and provider resolution / 404 precedence / the 503 no-provider
// contract are all unchanged; and never another object's events. ?events
// on a FILE keeps the key-scoped surface byte-identical to s3.
package webdav

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// partialEventsProvider is a key-AWARE provider double: it filters the
// stream by the queried key with the SAME rule the real zmetad provider
// applies (rowsMatchKey over rowEventSet, metadata/zmetad_paths.go):
// a RESOLVED row (non-NULL full_path) matches on exact key equality, a
// PARTIAL row (NULL full_path, bare name kept) matches conservatively via
// bare-name equality or the queried key ending in "/"+bare. key == "" is
// the bucket-scoped query: the whole stream, unfiltered (that is what the
// real provider does — the key filter is applied by the query, not by the
// bucket request).
//
// Reproducing the rule here is the point: the L2 leak is a property of
// asking for the WRONG key, so the double has to answer the right key the
// way the production provider would.
type partialEventsProvider struct {
	mu     sync.Mutex
	rows   []partialEventRow
	detail metadata.HistoryDetail
}

type partialEventRow struct {
	key     string // full_path when resolved, bare name when partial
	partial bool   // true = NULL full_path (PARTIAL, conservative match)
}

func (p *partialEventsProvider) Name() string { return "zfs-events" }

func (p *partialEventsProvider) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Available: true, Dataset: p.detail.Dataset}, nil
}

func (p *partialEventsProvider) History(_ context.Context, _, key string, q metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []metadata.ObjectEvent
	for i, row := range p.rows {
		if key != "" && !partialRowMatches(row, key) {
			continue
		}
		out = append(out, metadata.ObjectEvent{
			Op: "create", Key: row.key, Txg: uint64(1000 - i), SizeNew: int64(i + 1),
		})
		if q.MaxEvents > 0 && len(out) >= q.MaxEvents {
			break
		}
	}
	return out, nil
}

// partialRowMatches is metadata.rowsMatchKey's per-row key rule, verbatim
// in behavior: exact for a resolved row, conservative bare-name match for
// a PARTIAL row.
func partialRowMatches(row partialEventRow, key string) bool {
	if !row.partial {
		return row.key == key
	}
	return row.key == key || strings.HasSuffix(key, "/"+row.key)
}

func (p *partialEventsProvider) Purge(_ context.Context, _ string) error { return nil }

func (p *partialEventsProvider) LastDetail() metadata.HistoryDetail {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.detail
}

func (p *partialEventsProvider) configure(rows []partialEventRow, detail metadata.HistoryDetail) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows, p.detail = rows, detail
}

// newCollectionEventsEnv is newZFSSurfaceEnv plus a real nested collection
// whose last segment collides with an unrelated object's bare name — the
// exact shape the audit named (bucket holds an object a/report.txt; a
// PARTIAL row for a different object recorded the bare name "report").
func newCollectionEventsEnv(t *testing.T) (*zfsSurfaceEnv, *partialEventsProvider) {
	t.Helper()
	e := newZFSSurfaceEnv(t, "zfssurface-coll-bkt")
	be := s3.TestBackend()

	// The decoy: an object whose BASENAME (report.txt) collides with the
	// collection's last segment (report/).
	if _, err := be.Put(context.Background(), e.bucket, "a/report.txt",
		strings.NewReader("report-bytes"), int64(len("report-bytes")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("backend put a/report.txt: %v", err)
	}
	// A second object under the other collection, so both collections are
	// REAL collections (their prefixes list content) rather than 404s.
	if _, err := be.Put(context.Background(), e.bucket, "photos/2024/june.txt",
		strings.NewReader("june-bytes"), int64(len("june-bytes")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("backend put photos/2024/june.txt: %v", err)
	}
	if _, err := be.Put(context.Background(), e.bucket, "photos/report/inner.txt",
		strings.NewReader("inner-bytes"), int64(len("inner-bytes")), objectmodel.PutOptions{}); err != nil {
		t.Fatalf("backend put photos/report/inner.txt: %v", err)
	}
	// A plain directory so the collection kind is real on disk too.
	if err := os.MkdirAll(filepath.Join(e.bucketPath, "photos", "report"), 0o755); err != nil {
		t.Fatalf("mkdir photos/report: %v", err)
	}

	// The stream: one RESOLVED row per real object, plus the PARTIAL row
	// whose bare name "report" is what the suffix rule latches onto.
	prov := &partialEventsProvider{}
	prov.configure(
		[]partialEventRow{
			{key: "a/report.txt"},            // resolved
			{key: "photos/2024/june.txt"},    // resolved
			{key: "photos/report/inner.txt"}, // resolved
			{key: "report", partial: true},   // PARTIAL: unknown object, bare name only
		},
		metadata.HistoryDetail{Dataset: "stub/coll"},
	)
	s3.InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == e.bucketPath {
			return prov
		}
		return nil
	})
	return e, prov
}

// eventsEnvelope is the parsed ?events JSON wire shape (the envelope the
// s3 surface writes, read locally so field drift on the wire fails).
type eventsEnvelope struct {
	Dataset     string `json:"dataset"`
	RecordsLost uint64 `json:"recordsLost"`
	RingSwaps   uint64 `json:"ringSwaps"`
	Events      []struct {
		Op  string `json:"op"`
		Key string `json:"key"`
		Txg uint64 `json:"txg"`
	} `json:"events"`
}

// decodeEvents parses the ?events JSON envelope.
func decodeEvents(t *testing.T, body string) eventsEnvelope {
	t.Helper()
	var out eventsEnvelope
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal ?events body %q: %v", body, err)
	}
	return out
}

// TestZFSSurfaceEventsNestedCollectionNeverLeaksAnotherObject is THE L2
// pin: ?events on a nested COLLECTION path answers the honest empty
// history, and never an unrelated object's events.
//
// Before the fix, /photos/report/?events routed to the key-scoped surface
// with objectName="photos/report"; the PARTIAL-row suffix rule then
// returned the bare-name "report" row — an object this bucket path never
// named — alongside nothing else.
func TestZFSSurfaceEventsNestedCollectionNeverLeaksAnotherObject(t *testing.T) {
	e, _ := newCollectionEventsEnv(t)

	// Sanity: the bucket ?events surface DOES see every row (so a leak
	// below cannot be explained by an empty provider).
	_, _, body := e.davGet("/?events")
	if !strings.Contains(body, `"key":"report"`) {
		t.Fatalf("bucket ?events must list the PARTIAL bare-name row (test fixture broken): %s", body)
	}

	for _, tc := range []struct {
		name   string
		target string
		modeA  bool
	}{
		// The audit's own example: a bucket holds an object recorded under
		// the bare name "report"; the collection's last segment is
		// "report" too. Its events must not surface here.
		{"basename collision, mode B", "/photos/report/?events", false},
		{"basename collision, mode A", "/zfssurface-coll-bkt/photos/report/?events", true},
		{"plain nested collection, mode B", "/photos/2024/?events", false},
		{"plain nested collection, mode A", "/zfssurface-coll-bkt/photos/2024/?events", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get := e.davGet
			if tc.modeA {
				get = e.davAGet
			}
			code, hdr, body := get(tc.target)
			if code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 (the query is known, the bucket exists): %s", tc.target, code, body)
			}
			if ct := hdr.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("%s: content-type %q, want application/json", tc.target, ct)
			}
			hist := decodeEvents(t, body)
			if len(hist.Events) != 0 {
				t.Fatalf("%s: a collection path must answer the EMPTY event history, got %d event(s): %s",
					tc.target, len(hist.Events), body)
			}
			// The envelope stays honest (dataset surfaced, nothing fabricated).
			if hist.Dataset != "stub/coll" {
				t.Fatalf("%s: dataset = %q, want stub/coll (envelope must stay honest): %s", tc.target, hist.Dataset, body)
			}
		})
	}
}

// TestZFSSurfaceEventsCollectionLeakIsNotAFixtureArtifact proves the L2
// leak was REAL, not a fixture accident: on the very same bucket, provider
// and keys, the FILE paths still return exactly the key's own event — and
// the PARTIAL-row suffix rule really does fire for the key the collection
// path was wrongly asking with. So the empty answer above comes from the
// collection dispatch, not from the provider having nothing to say.
func TestZFSSurfaceEventsCollectionLeakIsNotAFixtureArtifact(t *testing.T) {
	e, _ := newCollectionEventsEnv(t)

	// The rule itself fires for the wrongly-queried key (this is the leak,
	// reproduced at the seam the real provider implements).
	if !partialRowMatches(partialEventRow{key: "report", partial: true}, "photos/report") {
		t.Fatal("fixture broken: the PARTIAL bare-name rule must match the queried key the collection path used")
	}
	// The file surfaces answer exactly their own key's events.
	for _, tc := range []struct{ target, wantKey string }{
		{"/a/report.txt?events", "a/report.txt"},
		{"/photos/2024/june.txt?events", "photos/2024/june.txt"},
		{"/photos/report/inner.txt?events", "photos/report/inner.txt"},
	} {
		code, _, body := e.davGet(tc.target)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", tc.target, code, body)
		}
		hist := decodeEvents(t, body)
		if len(hist.Events) != 1 || hist.Events[0].Key != tc.wantKey {
			t.Fatalf("%s: file ?events must return exactly %s's event, got %+v (%s)",
				tc.target, tc.wantKey, hist.Events, body)
		}
	}
	// HEAD mirrors GET on the same rule.
	req := httptest.NewRequest("HEAD", "/photos/report/?events", nil)
	w := httptest.NewRecorder()
	e.dav.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD on a collection ?events = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestZFSSurfaceEventsFileParityUnchangedOnTheSameBucket keeps the s3
// parity the leaf shipped: on this bucket the FILE ?events surfaces stay
// byte-identical to what the s3 pipeline answers for the same key. (The
// collection path has no s3 counterpart at all — that is the asymmetry
// this fix removes.)
func TestZFSSurfaceEventsFileParityUnchangedOnTheSameBucket(t *testing.T) {
	e, _ := newCollectionEventsEnv(t)

	for _, key := range []string{"a/report.txt", "photos/2024/june.txt", "photos/report/inner.txt"} {
		s3Code, _, s3Body := e.s3Get("/" + e.bucket + "/" + key + "?events")
		if s3Code != http.StatusOK {
			t.Fatalf("s3 baseline %s?events = %d: %s", key, s3Code, s3Body)
		}
		davCode, _, davBody := e.davGet("/" + key + "?events")
		if davCode != s3Code || davBody != s3Body {
			t.Fatalf("file ?events parity broke for %s (status %d vs %d):\n dav: %s\n s3:  %s",
				key, davCode, s3Code, davBody, s3Body)
		}
	}
}
