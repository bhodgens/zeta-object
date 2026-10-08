package s3_test

// events_cursor_test.go — gateway issue #15: the ?since-id pass-through on
// the ?events surfaces. Pins on the REAL s3 dispatch pipeline:
//
//   - ?events&since-id=N reaches the provider as HistoryQuery.SinceID and
//     the response entries carry the additive "id" field;
//   - invalid since-id (negative, non-numeric) -> 400 InvalidArgument;
//   - absent since-id is byte-identical to the pre-#15 response shape
//     (the parity gate: events from providers that record no ids marshal
//     without the id field at all);
//   - max-events semantics unchanged (the cap applies after the cursor).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// cursorStub is a HistoryQuery-recording provider double (the plain
// stubEventsProvider discards the query).
type cursorStub struct {
	mu     sync.Mutex
	query  metadata.HistoryQuery
	events []metadata.ObjectEvent
}

func (s *cursorStub) Name() string { return "zfs-events" }

func (s *cursorStub) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Available: true, Dataset: "stub/cursor"}, nil
}

func (s *cursorStub) History(_ context.Context, _, _ string, q metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.query = q
	events := s.events
	if q.SinceID > 0 {
		filtered := make([]metadata.ObjectEvent, 0, len(events))
		for _, e := range events {
			if e.ID > q.SinceID {
				filtered = append(filtered, e)
			}
		}
		events = filtered
	}
	if q.MaxEvents > 0 && len(events) > q.MaxEvents {
		events = events[:q.MaxEvents]
	}
	return events, nil
}

func (s *cursorStub) Purge(_ context.Context, _ string) error { return nil }

func (s *cursorStub) LastDetail() metadata.HistoryDetail {
	return metadata.HistoryDetail{Dataset: "stub/cursor"}
}

func (s *cursorStub) seenQuery() metadata.HistoryQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.query
}

// cursorEvents returns the canned cursor stream: ids 11..14 (monotonic,
// ascending), one per op class.
func cursorEvents() []metadata.ObjectEvent {
	return []metadata.ObjectEvent{
		{ID: 11, Op: "create", Key: "a.txt", Txg: 1, SizeNew: 10},
		{ID: 12, Op: "truncate", Key: "a.txt", Txg: 2, SizeNew: 20},
		{ID: 13, Op: "rename", Key: "b.txt", OldKey: "a.txt", Txg: 3},
		{ID: 14, Op: "remove", Key: "gone.txt", Txg: 4},
	}
}

// installCursorStub attaches stub for bucketPath via the exported
// wiring hook (production seam — no registry games), restoring nil on
// cleanup.
func installCursorStub(t *testing.T, stub *cursorStub, bucketPath string) {
	t.Helper()
	s3.InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == bucketPath {
			return stub
		}
		return nil
	})
	t.Cleanup(func() { s3.InstallMetadataProvider(nil) })
}

// TestEventsSinceIDPassThrough pins the s3 surface: the param parses into
// HistoryQuery.SinceID, the entries carry id, resume is exact.
func TestEventsSinceIDPassThrough(t *testing.T) {
	stub := &cursorStub{events: cursorEvents()}
	srv, root := newEventsTestServer(t)
	bucketPath := eventsAttachedBucket(t, srv, root, "cursorbkt", "a.txt", "x")
	installCursorStub(t, stub, bucketPath)

	code, _, body := eventsGet(t, srv, "/cursorbkt?events&since-id=12&max-events=1")
	if code != http.StatusOK {
		t.Fatalf("since-id events = %d, want 200 (body %s)", code, body)
	}
	seen := stub.seenQuery()
	if seen.SinceID != 12 {
		t.Fatalf("provider saw SinceID = %d, want 12", seen.SinceID)
	}
	if seen.MaxEvents != 1 {
		t.Fatalf("provider saw MaxEvents = %d, want 1 (max-events unchanged)", seen.MaxEvents)
	}
	var resp struct {
		Events []struct {
			ID  int64  `json:"id"`
			Op  string `json:"op"`
			Key string `json:"key"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	// stub filter: ids > 12 -> {13, 14}; max-events=1 -> exactly id 13.
	if len(resp.Events) != 1 || resp.Events[0].ID != 13 || resp.Events[0].Op != "rename" {
		t.Fatalf("resumed events = %+v (body %s), want exactly id 13 (rename)", resp.Events, body)
	}

	// Key-scoped surface: the same pass-through on /{bucket}/{key}?events.
	code, _, body = eventsGet(t, srv, "/cursorbkt/a.txt?events&since-id=10")
	if code != http.StatusOK {
		t.Fatalf("key-scoped since-id = %d, want 200 (body %s)", code, body)
	}
	if q := stub.seenQuery(); q.SinceID != 10 {
		t.Fatalf("key-scoped provider saw SinceID = %d, want 10", q.SinceID)
	}
	if !strings.Contains(body, `"id":11`) {
		t.Fatalf("key-scoped body missing the cursor id field: %s", body)
	}
}

// TestEventsSinceIDInvalid pins the 400 InvalidArgument contract.
func TestEventsSinceIDInvalid(t *testing.T) {
	stub := &cursorStub{events: cursorEvents()}
	srv, root := newEventsTestServer(t)
	bucketPath := eventsAttachedBucket(t, srv, root, "cursorbkt2", "a.txt", "x")
	installCursorStub(t, stub, bucketPath)

	// "-0" parses to 0 and an empty value is absent — both are the
	// legitimate no-cursor case, not errors.
	for _, bad := range []string{"-1", "abc", "1.5", "99999999999999999999", "1e3", "12x"} {
		code, _, body := eventsGet(t, srv, "/cursorbkt2?events&since-id="+bad)
		if code != http.StatusBadRequest {
			t.Fatalf("since-id=%q -> %d, want 400 (body %s)", bad, code, body)
		}
		if !strings.Contains(body, "<Code>InvalidArgument</Code>") {
			t.Fatalf("since-id=%q body missing InvalidArgument code: %s", bad, body)
		}
	}
	// A rejected cursor never reaches the provider (no silent fallback to
	// no-cursor: that would redeliver or skip events).
	if q := stub.seenQuery(); q.SinceID != 0 || q.MaxEvents != 0 {
		t.Fatalf("provider saw a query after an invalid cursor: %+v", q)
	}
}

// TestEventsSinceIDAbsentParity pins the byte-parity gate: an events
// stream from a provider that records NO ids marshals exactly as before
// #15 — no "id" key anywhere on the wire.
func TestEventsSinceIDAbsentParity(t *testing.T) {
	stub := &cursorStub{events: []metadata.ObjectEvent{
		{Op: "create", Key: "a.txt", Txg: 7, SizeNew: 10}, // ID left zero
	}}
	srv, root := newEventsTestServer(t)
	bucketPath := eventsAttachedBucket(t, srv, root, "paritybkt", "a.txt", "x")
	installCursorStub(t, stub, bucketPath)

	code, _, body := eventsGet(t, srv, "/paritybkt?events")
	if code != http.StatusOK {
		t.Fatalf("no-cursor events = %d, want 200 (body %s)", code, body)
	}
	if strings.Contains(body, `"id"`) {
		t.Fatalf("id-less events leaked an id field (parity break): %s", body)
	}
	if !strings.Contains(body, `"txg":7`) {
		t.Fatalf("body lost the pre-#15 fields: %s", body)
	}

	// With ids present, absent since-id still returns everything.
	stub.mu.Lock()
	stub.events = cursorEvents()
	stub.mu.Unlock()
	code, _, body = eventsGet(t, srv, "/paritybkt?events")
	if code != http.StatusOK {
		t.Fatalf("events = %d, want 200", code)
	}
	var resp struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(resp.Events) != 4 {
		t.Fatalf("absent since-id returned %d events, want 4", len(resp.Events))
	}
	for i, e := range resp.Events {
		if e.ID != int64(11+i) {
			t.Fatalf("events[%d].ID = %d, want %d", i, e.ID, 11+i)
		}
	}
}
