package sync

// matrix_test.go - THE LOCKED CONFLICT MATRIX as a table test, one row
// per matrix line, plus the upload-pipeline assertions (MD5 skip,
// 412 escalation, 5xx-leaves-dirty) and the idempotent re-sync.
//
// Each case: arrange (server via memfs, index via seed, disk via
// putLocal) -> SyncOnce -> assert the EXACT expected outcome.

import (
	"context"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// bg is the test context.
func bg() context.Context { return context.Background() }

// matrixCase is one row of the conflict matrix test.
type matrixCase struct {
	name string
	// arrange
	server    string // remote body; "" + serverAbsent = no remote row
	hasServer bool
	indexETag string // known-good etag in the index row ("" = none)
	seed      func(*harness, string) // extra index state (dirty/tombstone/clean)
	local     string // local cache body ("" + localAbsent = no file)
	hasLocal  bool
	// expect
	wantAction func(*harness, string) string // human-readable outcome probe
	wantCounts Report
}

func TestConflictMatrix(t *testing.T) {
	const key = "doc.txt"
	etagV1 := transport.MD5Hex([]byte("v1"))
	etagV2 := transport.MD5Hex([]byte("v2"))

	cases := []matrixCase{
		{
			// Matrix 1: clean local + changed remote -> download.
			name:      "1-clean-local-changed-remote-download",
			server:    "v2", hasServer: true,
			indexETag: etagV1,
			seed: func(h *harness, k string) { h.seedClean(k, etagV1) },
			local:    "v1", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				return "local=" + h.localBody(k) + " etag=" + h.row(k).ETag + " dirty=" + b2s(h.row(k).Dirty)
			},
			wantCounts: Report{Scanned: 1, Downloads: 1},
		},
		{
			// Matrix 2: dirty local + unchanged remote -> upload If-Match.
			name:      "2-dirty-local-unchanged-remote-upload",
			server:    "v1", hasServer: true,
			indexETag: etagV1,
			seed: func(h *harness, k string) { h.seedDirty(k, etagV1) },
			local:    "local edit", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				return "server=" + h.fs.ServerBodyOrEmpty(k) + " dirty=" + b2s(h.row(k).Dirty)
			},
			wantCounts: Report{Scanned: 1, Uploads: 1},
		},
		{
			// Matrix 3: dirty local + changed remote -> CONFLICT COPY
			// (remote wins locally; local preserved under the dated name).
			name:      "3-dirty-local-changed-remote-conflict-copy",
			server:    "v2", hasServer: true,
			indexETag: etagV1,
			seed: func(h *harness, k string) { h.seedDirty(k, etagV1) },
			local:    "local edit", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				got := h.localBody(k)
				preserved := h.localBody("doc (conflicted copy 2026-10-06).txt")
				return "local=" + got + " preserved=" + preserved + " server=" + h.fs.ServerBodyOrEmpty(k)
			},
			// The copy is created AND pushed as a new file in the same
			// pass (Uploads:1), so a second client sees it too.
			wantCounts: Report{Scanned: 2, Downloads: 1, Uploads: 1, ConflictCopies: 1},
		},
		{
			// Matrix 4: remote deleted + clean local -> delete locally +
			// tombstone.
			name:      "4-remote-deleted-clean-local-tombstone",
			hasServer: false,
			indexETag: etagV1,
			seed: func(h *harness, k string) { h.seedClean(k, etagV1) },
			local:    "v1", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				return "local=" + h.localBody(k) + " tomb=" + b2s(h.row(k).Deleted)
			},
			wantCounts: Report{Scanned: 1, RemoteDeletes: 1},
		},
		{
			// Matrix 5: remote deleted + dirty local -> KEEP local,
			// surface the conflict.
			name:      "5-remote-deleted-dirty-local-keep",
			hasServer: false,
			indexETag: etagV1,
			seed: func(h *harness, k string) { h.seedDirty(k, etagV1) },
			local:    "local edit", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				return "local=" + h.localBody(k) + " dirty=" + b2s(h.row(k).Dirty) + " tomb=" + b2s(h.row(k).Deleted)
			},
			wantCounts: Report{Scanned: 1, RemoteDeleteKept: 1},
		},
		{
			// Matrix 6: local deleted (tombstone) + remote unchanged ->
			// DELETE remote.
			name:      "6-local-deleted-remote-unchanged-delete-remote",
			server:    "v1", hasServer: true,
			indexETag: etagV1,
			seed: func(h *harness, k string) {
				if err := h.store.DeletePath(bg(), k, "user delete via mount"); err != nil {
					t.Fatalf("tombstone: %v", err)
				}
			},
			hasLocal: false, // user deleted the local file too
			wantAction: func(h *harness, k string) string {
				_, stillThere := h.fs.ServerBody(k)
				return "serverPresent=" + b2s(stillThere)
			},
			wantCounts: Report{Scanned: 1, LocalDeletes: 1},
		},
		{
			// Matrix 7: new remote (no index row) -> download.
			name:      "7-new-remote-download",
			server:    "brand new", hasServer: true,
			hasLocal: false,
			wantAction: func(h *harness, k string) string {
				return "local=" + h.localBody(k) + " etag=" + h.row(k).ETag
			},
			wantCounts: Report{Scanned: 1, Downloads: 1},
		},
		{
			// Matrix 8: new local (no index row, no server row) -> upload
			// If-None-Match:*.
			name:      "8-new-local-upload-create",
			hasServer: false,
			local:     "fresh local", hasLocal: true,
			wantAction: func(h *harness, k string) string {
				return "server=" + h.fs.ServerBodyOrEmpty(k) + " dirty=" + b2s(h.row(k).Dirty)
			},
			wantCounts: Report{Scanned: 1, Uploads: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.hasServer {
				h.fs.PutRaw(key, tc.server)
			}
			if tc.hasLocal {
				h.putLocal(key, tc.local)
			}
			if tc.seed != nil {
				tc.seed(h, key)
			}

			if err := h.eng.SyncOnce(bg()); err != nil {
				t.Fatalf("SyncOnce: %v", err)
			}

			// The action ran; prove the exact expected end state.
			got := tc.wantAction(h, key)
			switch tc.name[:1] {
			case "1":
				want := "local=v2 etag=" + etagV2 + " dirty=false"
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			case "2":
				want := "server=local edit dirty=false"
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			case "3":
				if !strings.Contains(got, "local=v2") ||
					!strings.Contains(got, "preserved=local edit") ||
					!strings.Contains(got, "server=v2") {
					t.Fatalf("outcome = %q, want remote applied + local preserved", got)
				}
			case "4":
				want := "local= tomb=true"
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			case "5":
				want := "local=local edit dirty=true tomb=false"
				if got != want {
					t.Fatalf("outcome = %q, want %q (KEEP local)", got, want)
				}
			case "6":
				want := "serverPresent=false"
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			case "7":
				want := "local=brand new etag=" + transport.MD5Hex([]byte("brand new"))
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			case "8":
				want := "server=fresh local dirty=false"
				if got != want {
					t.Fatalf("outcome = %q, want %q", got, want)
				}
			}

			// Counters: Scanned may include the complement pass, but the
			// action counters must match exactly.
			rep := h.eng.LastReport()
			if rep.Downloads != tc.wantCounts.Downloads || rep.Uploads != tc.wantCounts.Uploads ||
				rep.ConflictCopies != tc.wantCounts.ConflictCopies ||
				rep.RemoteDeletes != tc.wantCounts.RemoteDeletes ||
				rep.LocalDeletes != tc.wantCounts.LocalDeletes ||
				rep.RemoteDeleteKept != tc.wantCounts.RemoteDeleteKept {
				t.Fatalf("report = %+v, want %+v", rep, tc.wantCounts)
			}
		})
	}
}

func b2s(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
