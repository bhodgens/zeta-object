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

	"mini-s3/internal/backend"
	"mini-s3/internal/backend/fsbackend"
	"mini-s3/internal/frontend/s3"
	"mini-s3/internal/metadata"
)

// eventsAttachMarker marks a bucket root as provider-attached for the
// stub (per-bucket attach, like the real provider's FS-type probe).
const eventsAttachMarker = ".events-provider"

// stubEventsProvider is the leaf-04 test double for the zfs-events
// provider. Tests mutate its fields between cases (package tests run
// sequentially); the mutex keeps the race detector honest if the handler
// ever probes concurrently. History applies the MaxEvents cap the same
// way the real provider does, so the handler's query propagation is
// observable. LastDetail implements the optional detail seam the handler
// consults in preference to the package-level LastHistoryDetail hook.
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
