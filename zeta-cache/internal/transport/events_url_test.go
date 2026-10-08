package transport

// events_url_test.go — pins the ?events wire form (found live on
// zfs-meta): the query string MUST ride r.URL.RawQuery, never the escaped
// path — resourceURL path-escapes '?' to %3F and the gateway then answers
// 404 for a literal key named "?events&...". Events builds its own request
// and sets RawQuery; this test fails if that regresses to the key form.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEventsURLQueryForm(t *testing.T) {
	var gotPath, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		// Bucket-collection form: the path is the bucket root only.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dataset":"d","recordsLost":0,"ringSwaps":0,"events":[]}`))
	}))
	defer srv.Close()
	// httptest serves http:// — the Options validator accepts it for tests.
	cl, err := NewClient(Options{ServerURL: srv.URL, Bucket: "zval",
		BasicAuthUser: "u", BasicAuthPass: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Events(context.Background(), 0, 10); err != nil {
		t.Fatalf("Events: %v", err)
	}
	if gotPath != "/zval/" {
		t.Errorf("events must target the bucket root collection, got path %q", gotPath)
	}
	if !strings.Contains(gotRawQuery, "events=") || !strings.Contains(gotRawQuery, "since-id=0") || !strings.Contains(gotRawQuery, "max-events=10") {
		t.Errorf("events query must ride RawQuery (events/since-id/max-events), got %q", gotRawQuery)
	}
	if strings.Contains(gotPath, "%3F") || strings.Contains(gotPath, "events") {
		t.Errorf("query must never leak into the path (the %%3F 404 bug), got %q", gotPath)
	}
}
