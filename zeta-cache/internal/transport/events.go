package transport

// events.go — the ?events wire seam (client-cache leaf 05, consuming the
// gateway's since-id pass-through, zeta-object#15). The events surface is
// an ENRICHMENT probe, never a correctness input (master decision 7): a
// 503 NotImplemented (plain bucket, no zmetad provider) is a STATUS the
// caller falls back from (full PROPFIND scan), not an error.
//
// Wire contract (zeta-object#15): GET <bucket>?events&since-id=N&max-events=M
// answers 200 with JSON {dataset, recordsLost, ringSwaps, events:[...]},
// each event carrying its monotonic row `id` — the cursor. The query is
// the bucket-collection form, so it rides the bucket URL (key "").

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// EventsNotAvailableError marks the contracted 503: the bucket has no
// metadata provider (plain-dir storage). Callers treat it as "cursor
// disabled, scan-only" — the PERMANENT behavior for non-ZFS buckets,
// never a retry condition.
type EventsNotAvailableError struct{}

func (EventsNotAvailableError) Error() string {
	return "transport: no events provider for this bucket (503 NotImplemented)"
}

// Event is one entry of the ?events JSON stream (the #15 wire shape).
// ID is the monotonic cursor; a server that does not surface ids leaves
// it 0 and the cursor feed must treat the page as un-cursored (full scan).
type Event struct {
	ID        int64  `json:"id"`
	Op        string `json:"op"`
	Key       string `json:"key"`
	OldKey    string `json:"oldKey,omitempty"`
	Txg       uint64 `json:"txg"`
	Timestamp string `json:"timestamp"`
	SizeOld   int64  `json:"sizeOld,omitempty"`
	SizeNew   int64  `json:"sizeNew,omitempty"`
}

// EventHistory is the ?events envelope. RecordsLost and RingSwaps are the
// loss counters the cursor feed invalidates on (either advancing past the
// last-seen values means events were DROPPED upstream: full rescan).
type EventHistory struct {
	Dataset     string  `json:"dataset"`
	RecordsLost uint64  `json:"recordsLost"`
	RingSwaps   uint64  `json:"ringSwaps"`
	Events      []Event `json:"events"`
}

// Events fetches the bucket's event history with the since-id cursor.
// sinceID < 0 means "no cursor" (the plain ?events probe). maxEvents <= 0
// takes the server default. A 503 answers (nil, EventsNotAvailable); any
// other non-200 is a hard error.
//
// The query rides RawQuery, NEVER the resourceURL key: the key form
// "?events&..." is path-escaped (%3F) by resourceURL and the gateway then
// routes a literal key named "?events&..." — a live-validated 404 on
// zfs-meta. RawQuery keeps the request in the bucket-collection form the
// gateway's zfssurface dispatch expects.
func (c *Client) Events(ctx context.Context, sinceID int64, maxEvents int) (*EventHistory, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("events", "")
	if sinceID >= 0 {
		q.Set("since-id", strconv.FormatInt(sinceID, 10))
	}
	if maxEvents > 0 {
		q.Set("max-events", strconv.Itoa(maxEvents))
	}
	req.URL.RawQuery = q.Encode()
	c.doAuth(req)
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, EventsNotAvailableError{}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transport: events: unexpected HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		return nil, fmt.Errorf("transport: events: reading body: %w", err)
	}
	var hist EventHistory
	if err := json.Unmarshal(body, &hist); err != nil {
		return nil, fmt.Errorf("transport: events: response parse: %w", err)
	}
	return &hist, nil
}
