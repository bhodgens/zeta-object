package s3_test

// events_owner_test.go — ?events owner enrichment tests (auth extensions
// leaf 10): the owner field appears ONLY when the object's xattr exists;
// unstamped objects keep the exact pre-change shape (parity gate).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// TestEventsOwnerEnrichmentPresentAndAbsent pins both sides of the parity
// contract: a stamped object's create event carries owner=alice; a plain
// (unstamped) object's events carry NO owner key at all — byte-shape
// identical to the pre-change envelope.
func TestEventsOwnerEnrichmentPresentAndAbsent(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure([]metadata.ObjectEvent{
		{Op: "create", Key: "stamped.txt", Txg: 7},
		{Op: "create", Key: "plain.txt", Txg: 8},
	}, metadata.HistoryDetail{Dataset: "probe/data"}, nil)
	srv, root := newEventsTestServer(t)

	// stamped.txt: written through the backend with a principal → owner
	// xattr. plain.txt: written without one (legacy path).
	fb, err := fsbackend.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if resp := doSigned(t, srv, "PUT", "/owner-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	principalPut := func(key, principal string) {
		t.Helper()
		if _, err := fb.Put(context.Background(), "owner-bkt", key,
			io.NopCloser(strings.NewReader("v")), 1,
			objectmodel.PutOptions{Principal: principal}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	principalPut("stamped.txt", "alice")
	principalPut("plain.txt", "")
	// Provider attach marker (the stub probes for it).
	if err := os.WriteFile(filepath.Join(root, "owner-bkt", eventsAttachMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, body := eventsGet(t, srv, "/owner-bkt/stamped.txt?events")
	if code != http.StatusOK {
		t.Fatalf("?events = %d", code)
	}
	var ev struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, body)
	}
	if len(ev.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(ev.Events))
	}
	stamped := ev.Events[0]
	if stamped["owner"] != "alice" {
		t.Fatalf("stamped event owner = %v, want alice", stamped["owner"])
	}
	plain := ev.Events[1]
	if _, has := plain["owner"]; has {
		t.Fatalf("unstamped event fabricated an owner: %v", plain)
	}
}

// TestEventsOwnerParityEnvelope pins the envelope shape: the top-level key
// set never changes (owner lives on EVENTS only).
func TestEventsOwnerParityEnvelope(t *testing.T) {
	stub := eventsStubFor(t)
	stub.configure([]metadata.ObjectEvent{{Op: "create", Key: "k", Txg: 1}},
		metadata.HistoryDetail{Dataset: "probe/data"}, nil)
	srv, root := newEventsTestServer(t)
	if resp := doSigned(t, srv, "PUT", "/parity-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if err := os.WriteFile(filepath.Join(root, "parity-bkt", eventsAttachMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, body := eventsGet(t, srv, "/parity-bkt/k?events")
	if code != http.StatusOK {
		t.Fatalf("?events = %d (%s)", code, body)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"dataset": true, "recordsLost": true, "ringSwaps": true, "events": true}
	if len(env) != len(want) {
		t.Fatalf("envelope keys drifted: %v", env)
	}
	for k := range want {
		if _, ok := env[k]; !ok {
			t.Fatalf("envelope missing %q: %v", k, env)
		}
	}
}
