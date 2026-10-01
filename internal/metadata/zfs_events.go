package metadata

// Shared event-history types and match helpers.
//
// The legacy zfs CLI event transport that used to live here was deleted
// when the zmetad SQLite database became the only read path
// (zmetad-provider-2026-09 leaf 04). What remains is consumed by the
// zmetad provider and its row mapper (zmetad_provider.go,
// zmetad_paths.go):
//
//   - HistoryDetail: the loss/source detail carriers read through the
//     frontend's structural detailReporter seam;
//   - plausibleWallClockTime: monotonic-hrtime -> wall-clock plausibility
//     gate (bughunt M2), used for legacy rows with NULL captured_at;
//   - bareMatchesKey: the conservative PARTIAL-row key match reused by
//     rowsMatchKey.
//
// The recorded-fixture parser (parseEventsOutput and its graph-walk
// machinery) lives in zfs_events_parser_test.go: it is TEST-ONLY
// infrastructure for the leaf-02 drift guard, never linked into the
// production binary.

import (
	"strings"
	"time"
)

// HistoryDetail carries information ObjectEvent cannot: lossiness of the
// event history and the source dataset.
//
// Detail is served per provider INSTANCE through the frontend's
// structural detailReporter seam (LastDetail), never through a package
// global: a global last-detail record cross-attributes dataset/loss
// between concurrent ?events requests on different buckets (bughunt
// A2/C2). The frozen MetadataProvider interface MUST NOT gain methods
// (master Contract 1), hence the structural read.
type HistoryDetail struct {
	Dataset     string
	RecordsLost uint64 // nonzero = history is lossy; surfaces records_lost
	// RingSwaps counts kernel-log identity swaps (zmetad gaps rows with
	// the lost=-1 sentinel). Per SCHEMA.md section 4, swaps are epoch-
	// boundary COUNTS, not lost records: they MUST never be folded into
	// RecordsLost.
	RingSwaps uint64
}

// plausibleWallClockTime maps a monotonic hrtime value (ns) to a
// wall-clock timestamp ONLY when the value is plausible wall-clock time -
// after 2001-01-01 UTC (unix ns ≈ 0.98e18). (bughunt M2 fix) gethrtime()
// output is high-resolution and BOOT-RELATIVE on some platforms:
// serializing such a value as a wall-clock date fabricates a date in 1970
// (or earlier) and silently breaks the Since filter (every boot-relative
// timestamp reads as "older than Since"). Such values are left as the
// zero time instead - the wire contract documents zero = unknown, and the
// Since filter passes zero-timestamp events ("unknown" is not "older than
// Since"). The zmetad row mapper uses this for legacy rows whose
// captured_at is NULL (zmetad_paths.go).
func plausibleWallClockTime(ns uint64) time.Time {
	const minWallClockNS = uint64(978307200000000000) // 2001-01-01T00:00:00Z in unix ns
	if ns < minWallClockNS {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)) //nolint:gosec // G115: values >= 0.98e18 fit int64 with headroom to 2262
}

// bareMatchesKey is the conservative PARTIAL-row match: exact bare-name
// equality, or the queried key ending in "/"+bare. Showing a
// possibly-unrelated event beats silently hiding the only record of an
// object (the F-live-1 failure mode). rowsMatchKey (zmetad_paths.go)
// applies it to rows whose full_path is NULL; resolved rows match on
// exact full-path equality instead.
func bareMatchesKey(bare, key string) bool {
	return bare == key || strings.HasSuffix(key, "/"+bare)
}
