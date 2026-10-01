package s3_test

// capability_endpoints_test.go — metadata-zfs leaf 04: the ?events
// capability endpoints (object-level GET /{bucket}/{key}?events,
// bucket-level GET /{bucket}?events, and the ?events&versions XML
// extension). All tests drive the real dispatch pipeline through
// Frontend.Handler() with signed SigV4 requests, so auth inheritance and
// routing placement are exercised exactly as production sees them.
//
// Provider seam: the stub registers under the reserved name
// "zfs-events" (guarded — duplicate Register panics) and reports
// Available only for bucket roots carrying the eventsAttachMarker file,
// mirroring how the real zfs-events provider probes per-bucket FS type.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// eventsAttachMarker marks a bucket root as provider-attached for the
// stub (per-bucket attach, like the real provider's FS-type probe).
const eventsAttachMarker = ".events-provider"

// stubEventsProvider is the leaf-04 test double for the zfs-events
// provider. Tests mutate its fields between cases (package tests run
// sequentially); the mutex keeps the race detector honest if the handler
// ever probes concurrently. History applies the MaxEvents cap the same
// way the real provider does, so the handler's query propagation is
// observable. LastDetail implements the structural detailReporter seam
// the handler consults (the package-global LastHistoryDetail hook was
// deleted with the CLI transport, zmetad-provider-2026-09 leaf 04).
type stubEventsProvider struct {
	mu     sync.Mutex
	events []metadata.ObjectEvent
	detail metadata.HistoryDetail
	err    error
}

func (s *stubEventsProvider) Name() string { return "zfs-events" }

func (s *stubEventsProvider) Probe(_ context.Context, bucketPath string) (metadata.ProbeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(bucketPath, eventsAttachMarker)); err != nil {
		return metadata.ProbeResult{Available: false, Reason: "no provider marker"}, nil
	}
	return metadata.ProbeResult{Available: true, Dataset: "stub/data"}, nil
}

func (s *stubEventsProvider) History(_ context.Context, _, _ string, q metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	events := s.events
	if q.MaxEvents > 0 && len(events) > q.MaxEvents {
		events = events[:q.MaxEvents]
	}
	return events, nil
}

func (s *stubEventsProvider) Purge(_ context.Context, _ string) error { return nil }

func (s *stubEventsProvider) LastDetail() metadata.HistoryDetail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detail
}

func (s *stubEventsProvider) configure(events []metadata.ObjectEvent, detail metadata.HistoryDetail, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events, s.detail, s.err = events, detail, err
}

// eventsStubFor returns the process-wide stub, registering it on first
// use (Register panics on duplicates, so the guard is required).
func eventsStubFor(t *testing.T) *stubEventsProvider {
	t.Helper()
	if existing := metadata.Lookup("zfs-events"); existing != nil {
		stub, ok := existing.(*stubEventsProvider)
		if !ok {
			t.Fatalf("zfs-events registered by foreign test: %T", existing)
		}
		return stub
	}
	stub := &stubEventsProvider{}
	metadata.Register(stub)
	return stub
}

// newEventsTestServer mounts a frontend over a fresh fs backend and
// returns the server plus the backend root (for bucket/marker setup).
func newEventsTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	f, err := fsbackend.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s3.InstallServerConfigView(s3.ServerConfigView{Buckets: map[string]string{}, DataDir: root + "/"})
	s3.InstallFSRootResolver(func(bucket string) string {
		return filepath.Join(root, bucket)
	})
	s3.InstallBackendLookup(func(bucket string) (backend.Backend, error) { return f, nil })
	front := s3.New(f, s3.WithCredentialSource(staticCreds{"minioadmin": "minioadmin"}))
	srv := httptest.NewServer(front.Handler())
	t.Cleanup(srv.Close)
	return srv, root
}

// eventsAttachedBucket creates bucket with one object and the provider
// attach marker, returning the bucket root path.
func eventsAttachedBucket(t *testing.T, srv *httptest.Server, root, bucket, key, body string) string {
	t.Helper()
	if resp := doSigned(t, srv, "PUT", "/"+bucket, ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket %s: got %d", bucket, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doSigned(t, srv, "PUT", "/"+bucket+"/"+key, body); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("put %s: got %d", key, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	bucketRoot := filepath.Join(root, bucket)
	if err := os.WriteFile(filepath.Join(bucketRoot, eventsAttachMarker), nil, 0o644); err != nil {
		t.Fatalf("write attach marker: %v", err)
	}
	return bucketRoot
}

// getBody drains a response and returns status, headers snapshot and body.
func eventsGet(t *testing.T, srv *httptest.Server, target string) (int, http.Header, string) {
	t.Helper()
	resp := doSigned(t, srv, "GET", target, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// TestEventsDispatchRoutes pins the routing contract: the ?events
// sub-resource routes after auth (unsigned/unknown-key requests never
// reach it), unknown buckets still 404, and an existing bucket without
// an attached provider gets the contracted 503 NotImplemented.
func TestEventsDispatchRoutes(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(nil, metadata.HistoryDetail{}, nil)
	srv, root := newEventsTestServer(t)

	// Unknown bucket + object: 404 wins over provider resolution.
	if code, _, body := eventsGet(t, srv, "/no-such-bucket/some/key?events"); code != http.StatusNotFound {
		t.Fatalf("unknown bucket?events = %d, want 404 (body %s)", code, body)
	}
	// Unknown bucket, bucket-level: 404.
	if code, _, body := eventsGet(t, srv, "/no-such-bucket?events"); code != http.StatusNotFound {
		t.Fatalf("unknown bucket?events (bucket-level) = %d, want 404 (body %s)", code, body)
	}

	// Existing plain bucket (no attach marker): 503 NotImplemented.
	if resp := doSigned(t, srv, "PUT", "/plainbucket", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create plainbucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	_ = root
	if code, _, body := eventsGet(t, srv, "/plainbucket?events"); code != http.StatusServiceUnavailable {
		t.Fatalf("no-provider ?events = %d, want 503 (body %s)", code, body)
	}
	if code, _, body := eventsGet(t, srv, "/plainbucket/notexist?events"); code != http.StatusServiceUnavailable {
		t.Fatalf("no-provider object ?events = %d, want 503 (body %s)", code, body)
	}

	// SigV4 still enforced: an unsigned ?events request is rejected
	// before routing (the handler pipeline never sees it).
	resp, err := srv.Client().Get(srv.URL + "/plainbucket?events")
	if err != nil {
		t.Fatalf("unsigned request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned ?events = %d, want 403 (auth must precede dispatch)", resp.StatusCode)
	}
}

// TestObjectEventsHandler covers the object-level JSON contract:
// Content-Type application/json, envelope fields, lowercase op.
func TestObjectEventsHandler(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "create", Key: "a.txt", Txg: 1234, SizeNew: 100},
		},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "evtbkt", "a.txt", "hello")

	code, header, body := eventsGet(t, srv, "/evtbkt/a.txt?events")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q, want application/json", ct)
	}
	var resp struct {
		Dataset     string `json:"dataset"`
		RecordsLost uint64 `json:"recordsLost"`
		Events      []struct {
			Op  string `json:"op"`
			Key string `json:"key"`
			Txg uint64 `json:"txg"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if resp.Dataset != "stub/data" || resp.RecordsLost != 0 {
		t.Fatalf("envelope = %+v (body %s)", resp, body)
	}
	if len(resp.Events) != 1 || resp.Events[0].Op != "create" || resp.Events[0].Key != "a.txt" || resp.Events[0].Txg != 1234 {
		t.Fatalf("events = %+v (body %s)", resp.Events, body)
	}
}

// TestObjectEventsExecFailureNoStderrLeak pins the InternalError mapping:
// provider errors never leak zfs/exec detail to the client.
func TestObjectEventsExecFailureNoStderrLeak(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(nil, metadata.HistoryDetail{},
		errors.New("exit status 1: cannot get events for 'tank': secrets-here"))
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "failbkt", "a.txt", "x")

	code, _, body := eventsGet(t, srv, "/failbkt/a.txt?events")
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (body %s)", code, body)
	}
	if strings.Contains(body, "secrets-here") {
		t.Fatalf("provider error detail leaked to client: %s", body)
	}
	if !strings.Contains(body, "InternalError") {
		t.Fatalf("body %s missing InternalError code", body)
	}
}

// TestBucketEventsSummary covers bucket-level ?events: same JSON
// envelope, RecordsLost surfaced per master decision 4.
func TestBucketEventsSummary(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{{Op: "create", Key: "a", Txg: 1}},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 3},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "sumbkt", "a", "x")

	code, header, body := eventsGet(t, srv, "/sumbkt?events")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q, want application/json", ct)
	}
	var resp struct {
		Dataset     string `json:"dataset"`
		RecordsLost uint64 `json:"recordsLost"`
		Events      []struct {
			Op string `json:"op"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if resp.Dataset != "stub/data" {
		t.Fatalf("dataset = %q, want stub/data", resp.Dataset)
	}
	if resp.RecordsLost != 3 { // RecordsLost surfaced per master decision 4
		t.Fatalf("recordsLost = %d, want 3", resp.RecordsLost)
	}
	if len(resp.Events) != 1 || resp.Events[0].Op != "create" {
		t.Fatalf("events = %+v (body %s)", resp.Events, body)
	}
}

// TestVersionsExtensionXML pins the ?events&versions XML extension:
// pinned s3 namespace, newest-first per-key versions, delete markers,
// IsLossy/RecordsLost, distinct VersionIds.
func TestVersionsExtensionXML(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "truncate", Key: "a.txt", Txg: 30, SizeNew: 300},
			{Op: "create", Key: "a.txt", Txg: 10, SizeNew: 100},
			{Op: "remove", Key: "gone.txt", Txg: 20},
			{Op: "setattr", Key: "a.txt", Txg: 40}, // no version entry in v1
		},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 2},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "verbkt", "a.txt", "x")

	code, header, body := eventsGet(t, srv, "/verbkt?events&versions")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Fatalf("content-type %q, want xml", ct)
	}
	var got struct {
		XMLName     xml.Name `xml:"ListObjectVersionsExt"`
		Name        string   `xml:"Name"`
		IsLossy     bool     `xml:"IsLossy"`
		RecordsLost uint64   `xml:"RecordsLost"`
		Version     []struct {
			Key            string `xml:"Key"`
			VersionId      string `xml:"VersionId"`
			IsLatest       bool   `xml:"IsLatest"`
			LastModified   string `xml:"LastModified"`
			Size           int64  `xml:"Size"`
			Op             string `xml:"Op"`
			IsDeleteMarker bool   `xml:"IsDeleteMarker"`
		} `xml:"Version"`
	}
	if err := xml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Name != "verbkt" {
		t.Fatalf("Name = %q, want verbkt", got.Name)
	}
	if !got.IsLossy || got.RecordsLost != 2 {
		t.Fatalf("loss flag: %+v", got)
	}
	var aVersions []struct {
		Key            string `xml:"Key"`
		VersionId      string `xml:"VersionId"`
		IsLatest       bool   `xml:"IsLatest"`
		LastModified   string `xml:"LastModified"`
		Size           int64  `xml:"Size"`
		Op             string `xml:"Op"`
		IsDeleteMarker bool   `xml:"IsDeleteMarker"`
	}
	for _, v := range got.Version {
		if v.Key == "a.txt" {
			aVersions = append(aVersions, v)
		}
	}
	if len(aVersions) != 2 || aVersions[0].VersionId == aVersions[1].VersionId {
		t.Fatalf("a.txt versions = %+v (body %s)", aVersions, body)
	}
	if aVersions[0].Size != 300 || !aVersions[0].IsLatest {
		t.Fatalf("newest first broken: %+v", aVersions[0])
	}
	if aVersions[1].IsLatest {
		t.Fatalf("older version marked latest: %+v", aVersions[1])
	}
	if aVersions[0].LastModified != "" {
		t.Fatalf("LastModified must stay empty under the current wire format, got %q", aVersions[0].LastModified)
	}
	var gone struct {
		Key            string `xml:"Key"`
		IsDeleteMarker bool   `xml:"IsDeleteMarker"`
		IsLatest       bool   `xml:"IsLatest"`
	}
	for _, v := range got.Version {
		if v.Key == "gone.txt" {
			gone.Key, gone.IsDeleteMarker, gone.IsLatest = v.Key, v.IsDeleteMarker, v.IsLatest
		}
	}
	if gone.Key == "" || !gone.IsDeleteMarker {
		t.Fatalf("remove not marked: %+v (body %s)", gone, body)
	}
	if !gone.IsLatest {
		t.Fatalf("remove should be latest for its key: %+v", gone)
	}
}

// TestEventsDoNotDisturbCoreResponses is the handler-level parity
// invariant: ?events on a provider-attached bucket coexists with normal
// GET, mutates no core state, and honors the max-events cap.
func TestEventsDoNotDisturbCoreResponses(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "create", Key: "data.txt", Txg: 1, SizeNew: 13},
			{Op: "truncate", Key: "data.txt", Txg: 2, SizeNew: 20},
		},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "parbkt", "data.txt", "payload-bytes")

	code1, hdr1, body1 := eventsGet(t, srv, "/parbkt/data.txt")
	if code1 != http.StatusOK {
		t.Fatalf("plain GET = %d (body %s)", code1, body1)
	}

	// Capability on the same object.
	code2, hdr2, body2 := eventsGet(t, srv, "/parbkt/data.txt?events")
	if code2 != http.StatusOK || hdr2.Get("Content-Type") != "application/json" {
		t.Fatalf("?events = %d ct %q (body %s)", code2, hdr2.Get("Content-Type"), body2)
	}

	// Core response unchanged afterwards.
	code3, hdr3, body3 := eventsGet(t, srv, "/parbkt/data.txt")
	if code3 != code1 || body3 != body1 {
		t.Fatalf("core GET drifted after ?events: %d/%q vs %d/%q", code3, body3, code1, body1)
	}
	for _, h := range []string{"ETag", "Content-Type", "Content-Length"} {
		if hdr3.Get(h) != hdr1.Get(h) {
			t.Fatalf("header %s drifted: %q vs %q", h, hdr3.Get(h), hdr1.Get(h))
		}
	}

	// max-events caps the returned array (propagated as HistoryQuery.MaxEvents).
	code4, _, body4 := eventsGet(t, srv, "/parbkt/data.txt?events&max-events=1")
	if code4 != http.StatusOK {
		t.Fatalf("max-events GET = %d (body %s)", code4, body4)
	}
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(body4), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", body4, err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("max-events=1 returned %d events, want 1 (body %s)", len(resp.Events), body4)
	}

	// Bucket-level ?events&versions does NOT collide with plain ?versions
	// routing (the events check must win for the combined query).
	stub.configure(
		[]metadata.ObjectEvent{{Op: "create", Key: "data.txt", Txg: 1}},
		metadata.HistoryDetail{Dataset: "stub/data"},
		nil,
	)
	if code, _, body := eventsGet(t, srv, "/parbkt?events&versions"); code != http.StatusOK || !strings.Contains(body, "ListObjectVersionsExt") {
		t.Fatalf("?events&versions = %d (body %s)", code, body)
	}
}

// TestEventsJSONNoOwnerFields pins C8 (bughunt-gateway-2026-09-29):
// the ?events JSON wire form never exposes uid/gid (owner identity is
// unnecessary client surface). Set-UID/GID events are included to prove
// the fields stay absent even when the provider carries them.
func TestEventsJSONNoOwnerFields(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "create", Key: "a.txt", Txg: 1, SizeNew: 5, UID: 501, GID: 20},
			{Op: "setattr", Key: "a.txt", Txg: 2, UID: 0, GID: 0},
		},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "ownbkt", "a.txt", "hello")

	for _, target := range []string{"/ownbkt?events", "/ownbkt/a.txt?events"} {
		code, _, body := eventsGet(t, srv, target)
		if code != http.StatusOK {
			t.Fatalf("%s = %d (body %s)", target, code, body)
		}
		for _, leak := range []string{`"uid"`, `"gid"`} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s body exposes %s: %s", target, leak, body)
			}
		}
		// Sanity: the envelope still carries the real event fields.
		if !strings.Contains(body, `"op"`) || !strings.Contains(body, `"txg"`) {
			t.Fatalf("%s body lost core event fields: %s", target, body)
		}
	}
}

// TestEventsTraversalBucketRejected pins D3 (bughunt-postF2-2026-09-29):
// a path-traversal bucket name reaching ?events must get the standard 404
// NoSuchBucket — never a 200/503/500 from a provider probe that escaped
// dataDir. Regression: resolveEventsContext checked only bucketExists,
// whose filepath.Join raw name let /..%2f..%2fetc resolve to a real
// directory outside dataDir. Both bucket-level and object-level ?events
// are pinned, and a normal bucket still resolves.
func TestEventsTraversalBucketRejected(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(nil, metadata.HistoryDetail{}, nil)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "travbkt", "a.txt", "x")

	for _, target := range []string{
		"/..%2f..%2fetc?events",
		"/..%2f..%2fetc/passwd?events",
		"/.%2e%2ftravbkt?events",
	} {
		code, _, body := eventsGet(t, srv, target)
		if code != http.StatusNotFound {
			t.Fatalf("traversal %s = %d, want 404 (body %s)", target, code, body)
		}
		if !strings.Contains(body, "NoSuchBucket") {
			t.Fatalf("traversal %s body %s missing NoSuchBucket", target, body)
		}
	}

	// Normal bucket still works (bucket-level and object-level).
	if code, _, body := eventsGet(t, srv, "/travbkt?events"); code != http.StatusOK {
		t.Fatalf("normal bucket ?events = %d (body %s)", code, body)
	}
	if code, _, body := eventsGet(t, srv, "/travbkt/a.txt?events"); code != http.StatusOK {
		t.Fatalf("normal bucket object ?events = %d (body %s)", code, body)
	}
}

// TestEventsVersionsPrefixFilter pins D4 (bughunt-postF2-2026-09-29):
// ?events&versions honors the prefix query parameter — only events whose
// reconstructed key carries the prefix produce version entries. The
// endpoint doc also documents delimiter/encoding-type as not honored.
func TestEventsVersionsPrefixFilter(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{
			{Op: "create", Key: "a/one.txt", Txg: 10, SizeNew: 10},
			{Op: "create", Key: "b/two.txt", Txg: 11, SizeNew: 20},
			{Op: "create", Key: "a/one.txt", Txg: 12, SizeNew: 30},
			{Op: "remove", Key: "b/gone.txt", Txg: 13},
		},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "prefbkt", "a/one.txt", "x")

	code, _, body := eventsGet(t, srv, "/prefbkt?events&versions&prefix=a/")
	if code != http.StatusOK {
		t.Fatalf("status %d (body %s)", code, body)
	}
	var got struct {
		Version []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"Version"`
	}
	if err := xml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(got.Version) != 2 {
		t.Fatalf("versions = %+v, want exactly the 2 a/ entries (body %s)", got.Version, body)
	}
	for _, v := range got.Version {
		if !strings.HasPrefix(v.Key, "a/") {
			t.Fatalf("non-prefixed key leaked: %q (body %s)", v.Key, body)
		}
	}
	// Prefix boundary: "a/" must not swallow "ab"-prefixed keys.
	stub.configure(
		[]metadata.ObjectEvent{{Op: "create", Key: "ab.txt", Txg: 20, SizeNew: 5}},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0},
		nil,
	)
	if _, _, body := eventsGet(t, srv, "/prefbkt?events&versions&prefix=a/"); strings.Contains(body, "ab.txt") {
		t.Fatalf("prefix a/ matched ab.txt (body %s)", body)
	}
}

// TestInstallMetadataProviderHook pins the exported wiring entry: installing
// a resolver routes provider lookups through it, and installing nil restores
// the probe-on-request shim. (Wired from s3_wiring.go at startup; 0%-covered
// until this test - the CI coverage-floor gap fixed after the rename.)
func TestInstallMetadataProviderHook(t *testing.T) {
	called := ""
	s3.InstallMetadataProvider(func(bucketPath string) metadata.MetadataProvider {
		called = bucketPath
		return nil // nil provider is a valid resolution: no provider attached
	})
	if got := s3.MetadataProviderFor("/buckets/wired"); got != nil {
		t.Errorf("MetadataProviderFor = %v, want nil", got)
	}
	if called != "/buckets/wired" {
		t.Errorf("resolver called with %q, want /buckets/wired", called)
	}

	// nil restores the probe-on-request shim path (no registered provider
	// under test -> nil either way, but the hook-bypass branch must run).
	s3.InstallMetadataProvider(nil)
	if got := s3.MetadataProviderFor("/buckets/shim"); got != nil {
		t.Errorf("MetadataProviderFor after nil install = %v, want nil", got)
	}
}

// TestEventsRingSwapsWireFields pins the additive leaf-04 wire fields
// (zmetad-provider-2026-09 Contract 5): JSON envelope carries ringSwaps,
// XML carries RingSwaps, both from the provider's HistoryDetail, and
// IsLossy = RecordsLost > 0 OR RingSwaps > 0 - swaps are a separate loss
// class, never folded into RecordsLost.
func TestEventsRingSwapsWireFields(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure(
		[]metadata.ObjectEvent{{Op: "create", Key: "a.txt", Txg: 10, SizeNew: 5}},
		metadata.HistoryDetail{Dataset: "stub/data", RecordsLost: 0, RingSwaps: 3},
		nil,
	)
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "swapbkt", "a.txt", "x")

	// JSON envelope (object-level and bucket-level share the shape).
	for _, target := range []string{"/swapbkt?events", "/swapbkt/a.txt?events"} {
		code, _, body := eventsGet(t, srv, target)
		if code != http.StatusOK {
			t.Fatalf("%s = %d (body %s)", target, code, body)
		}
		if !strings.Contains(body, `"ringSwaps":3`) {
			t.Fatalf("%s body missing ringSwaps:3: %s", target, body)
		}
		if !strings.Contains(body, `"recordsLost":0`) {
			t.Fatalf("%s body missing recordsLost:0 (never folded): %s", target, body)
		}
		var env struct {
			Dataset     string `json:"dataset"`
			RecordsLost uint64 `json:"recordsLost"`
			RingSwaps   uint64 `json:"ringSwaps"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("unmarshal %q: %v", body, err)
		}
		if env.RingSwaps != 3 || env.RecordsLost != 0 || env.Dataset != "stub/data" {
			t.Fatalf("%s envelope = %+v, want ringSwaps 3 recordsLost 0", target, env)
		}
	}

	// XML extension: RingSwaps surfaced; IsLossy true from swaps alone.
	code, _, body := eventsGet(t, srv, "/swapbkt?events&versions")
	if code != http.StatusOK {
		t.Fatalf("versions = %d (body %s)", code, body)
	}
	var got struct {
		IsLossy     bool   `xml:"IsLossy"`
		RecordsLost uint64 `xml:"RecordsLost"`
		RingSwaps   uint64 `xml:"RingSwaps"`
	}
	if err := xml.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.RingSwaps != 3 {
		t.Fatalf("XML RingSwaps = %d, want 3 (body %s)", got.RingSwaps, body)
	}
	if got.RecordsLost != 0 {
		t.Fatalf("XML RecordsLost = %d, want 0 (swaps never folded)", got.RecordsLost)
	}
	if !got.IsLossy {
		t.Fatal("IsLossy must be true when RingSwaps > 0 (Contract 5)")
	}

	// No loss at all: IsLossy false, both counts zero.
	stub.configure(
		[]metadata.ObjectEvent{{Op: "create", Key: "a.txt", Txg: 10, SizeNew: 5}},
		metadata.HistoryDetail{Dataset: "stub/data"},
		nil,
	)
	code, _, body = eventsGet(t, srv, "/swapbkt?events&versions")
	if code != http.StatusOK {
		t.Fatalf("versions = %d (body %s)", code, body)
	}
	var clean struct {
		IsLossy     bool   `xml:"IsLossy"`
		RecordsLost uint64 `xml:"RecordsLost"`
		RingSwaps   uint64 `xml:"RingSwaps"`
	}
	if err := xml.Unmarshal([]byte(body), &clean); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if clean.IsLossy || clean.RecordsLost != 0 || clean.RingSwaps != 0 {
		t.Fatalf("clean history = %+v, want zero loss fields", clean)
	}
}

// TestHistoryDetailForZeroFallback pins the leaf-04 historyDetailFor
// fallback change: a provider WITHOUT the LastDetail structural seam gets
// a zero HistoryDetail (the package-global LastHistoryDetail fallback was
// deleted with the CLI transport - a global cross-attributes concurrent
// requests). The zero detail must render as an empty dataset and no loss
// claims on the JSON envelope.
type noDetailProvider struct{}

func (noDetailProvider) Name() string { return "zfs-events" }

func (noDetailProvider) Probe(_ context.Context, bucketPath string) (metadata.ProbeResult, error) {
	if _, err := os.Stat(filepath.Join(bucketPath, eventsAttachMarker)); err != nil {
		return metadata.ProbeResult{Available: false, Reason: "no provider marker"}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	return metadata.ProbeResult{Available: true, Dataset: "ignored/data"}, nil
}

func (noDetailProvider) History(_ context.Context, _, _ string, _ metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	return []metadata.ObjectEvent{{Op: "create", Key: "a.txt", Txg: 1}}, nil
}

func (noDetailProvider) Purge(_ context.Context, _ string) error { return nil }

func TestHistoryDetailForZeroFallback(t *testing.T) {
	// A provider without LastDetail: historyDetailFor must return the
	// zero value, not stale state from another bucket's request. Drive
	// through the installed hook so no registry surgery is needed.
	s3.InstallMetadataProvider(func(bucketPath string) metadata.MetadataProvider {
		if _, err := os.Stat(filepath.Join(bucketPath, eventsAttachMarker)); err != nil {
			return nil
		}
		return noDetailProvider{}
	})
	t.Cleanup(func() { s3.InstallMetadataProvider(nil) })
	srv, root := newEventsTestServer(t)
	eventsAttachedBucket(t, srv, root, "nodetail", "a.txt", "x")

	code, _, body := eventsGet(t, srv, "/nodetail?events")
	if code != http.StatusOK {
		t.Fatalf("status %d (body %s)", code, body)
	}
	var env struct {
		Dataset     string `json:"dataset"`
		RecordsLost uint64 `json:"recordsLost"`
		RingSwaps   uint64 `json:"ringSwaps"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if env.Dataset != "" || env.RecordsLost != 0 || env.RingSwaps != 0 {
		t.Fatalf("no-detail provider envelope = %+v, want the zero HistoryDetail", env)
	}
}
