package sync

// r3_cursor_zero_test.go - the R3 regression: the loss-invalidation Ack
// (pendingID==0) must NOT re-plant a literal "0" cursor row. The old Ack
// wrote MetaSet(cursor,"0") unconditionally: a crash after that Ack left
// a stored "0", which loadState parses as first contact again - an extra
// full scan on every restart until a new page arrived.

import (
	"context"
	"errors"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

func TestR3LossAckLeavesCursorAbsentNotZero(t *testing.T) {
	src := &stubEvents{pages: []transport.EventHistory{{
		RecordsLost: 5, RingSwaps: 1,
		Events: []transport.Event{{ID: 9, Op: "create", Key: "a"}},
	}}}
	f, st := newTestCursorFeed(t, src)
	ctx := context.Background()
	if _, _, err := deltaThenAck(ctx, f); err != nil {
		t.Fatalf("establish: %v", err)
	}
	if mustMeta(t, st, cursorMetaKey("bkt")) != "9" {
		t.Fatal("arrange: cursor not established")
	}

	// Loss advances: Delta invalidates (deletes the cursor) and answers
	// the authoritative full scan; Ack refreshes ONLY the loss baseline.
	src.pages = []transport.EventHistory{{
		RecordsLost: 6, RingSwaps: 2,
		Events: []transport.Event{{ID: 10, Op: "create", Key: "new.txt"}},
	}}
	paths, full, err := f.Delta(ctx)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("delta = (%v, %v), want authoritative full scan", paths, full)
	}
	if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("cursor meta after invalidation = %v, want deleted", err)
	}
	f.Ack(ctx) // the rescan completed

	// THE REGRESSION: the cursor meta must stay ABSENT - not a stale
	// "0" row. A crash after this Ack now resumes as a clean first
	// contact (one re-plant), never as first contact forced by a
	// parsed "0".
	if _, err := st.MetaGet(ctx, cursorMetaKey("bkt")); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("cursor meta after loss Ack = %v, want ABSENT (no replanted \"0\")", err)
	}
	// The loss baseline still refreshed.
	if got := mustMeta(t, st, lossMetaKey("bkt")); got != "6:2" {
		t.Fatalf("loss meta = %q, want 6:2", got)
	}

	// The next delta is a clean first contact and re-plants at the
	// newest id - WITHOUT the stale "0" ever having been stored.
	src.pages = []transport.EventHistory{{
		RecordsLost: 6, RingSwaps: 2,
		Events: []transport.Event{{ID: 10, Op: "create", Key: "new.txt"}},
	}}
	paths, full, err = deltaThenAck(ctx, f)
	if err != nil {
		t.Fatalf("re-establish Delta: %v", err)
	}
	if !full || paths != nil {
		t.Fatalf("re-establish = (%v, %v), want skip-and-replant", paths, full)
	}
	if got := mustMeta(t, st, cursorMetaKey("bkt")); got != "10" {
		t.Fatalf("cursor = %q, want re-established at 10", got)
	}
	// calls[0] = establish, calls[1] = the loss delta (still cursored
	// at 9 when it fetched), calls[2] = the first-contact refetch: it
	// must resume from 0, NOT from a stale replanted "0" (which would
	// behave the same here but forces the extra full scan on restart).
	if len(src.calls) < 3 {
		t.Fatalf("events calls = %v, want 3 (establish, loss, first contact)", src.calls)
	}
	if src.calls[1] != 9 {
		t.Fatalf("loss fetch used since-id %d, want 9", src.calls[1])
	}
	if src.calls[2] != 0 {
		t.Fatalf("first-contact refetch used since-id %d, want 0 (no stale cursor)", src.calls[2])
	}
}
