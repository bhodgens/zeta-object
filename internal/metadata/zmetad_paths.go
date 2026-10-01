package metadata

// Row -> ObjectEvent mapping for the zmetad v5 SQLite export
// (contrib/zmetad/SCHEMA.md sections 2.1 and 7). This is a THIN MAPPER,
// NOT a graph walk: on layout >= 5 zmetad resolves each row's dataset
// relative path at insert time and stores it in full_path, so mini-s3
// serves it directly with no reconstruction. The old CLI-path machinery
// (reconstructPaths, detectRoot, resolvePath and their txg-scoped maps)
// is deliberately not ported here.
//
// SCHEMA.md section 7 is THE contract:
//   - "a non-NULL full_path is authoritative as of the row's own event:
//     serve it directly, no reconstruction"
//   - a row whose full_path is NULL is PARTIAL: never fabricate a full
//     path for it (the ancestor chain cannot be proven) and never hide it
//     (it is still evidence the object existed). PARTIAL rows keep the
//     bare path name and match keys conservatively (see rowsMatchKey).
//
// Only bareMatchesKey and plausibleWallClockTime are reused from
// zfs_events.go; nothing here duplicates their logic.

import (
	"strings"
	"time"
)

// rowEventSet is rowsToEvents' output: the mapped ObjectEvents plus, per
// event, whether its Key (resolved) and OldKey (oldResolved) came from a
// non-NULL full_path/old_full_path. resolved[i]==true means events[i].Key
// is authoritative and matches exactly; false means the Key is a PARTIAL
// bare name that matches conservatively. Mirrors the legacy eventSet so
// the drift-guard test can compare the two pipelines field-for-field.
type rowEventSet struct {
	events      []ObjectEvent
	resolved    []bool // true iff full_path was non-NULL for events[i].Key
	oldResolved []bool // true iff old_full_path was non-NULL for events[i].OldKey
}

// rowsToEvents maps zmetad v5 EventRows to ObjectEvents per SCHEMA.md
// section 7 (see the file comment). Field mapping:
//
//   - Op: strings.ToLower(row.Op) - the DB stores schema enum NAMES
//     (CREATE -> create; UNKNOWN passes through lowercased).
//   - Key: *row.FullPath when non-NULL (resolved=true, served verbatim);
//     else *row.Path (bare, resolved=false). Never fabricated.
//   - OldKey: *row.OldFullPath when non-NULL (oldResolved=true); else
//     *row.OldPath (oldResolved=false).
//   - Txg: row.Txg.
//   - Timestamp: captured_at (wall clock) when present -> that unix second
//     UTC; else plausibleWallClockTime(row.Timestamp) (row.Timestamp is
//     monotonic hrtime, NOT wall clock - bughunt M2); else zero time
//     (= unknown on the wire). Never both.
//   - Sizes: TRUNCATE/SETATTR -> SizeNew from *row.Size, SizeOld from
//     *row.OldSize, each only when the pointer is present; WRITE/READ ->
//     SizeNew from *row.IoBytes when present (master Open Question 4
//     passthrough). All other ops: sizes stay zero (print_event parity).
//     NULL pointers are never fabricated into values.
//   - UID/GID: uint32(*row.UID/*row.GID) when present, else 0.
func rowsToEvents(rows []EventRow) *rowEventSet {
	set := &rowEventSet{
		events:      make([]ObjectEvent, 0, len(rows)),
		resolved:    make([]bool, len(rows)),
		oldResolved: make([]bool, len(rows)),
	}
	for i := range rows {
		r := rows[i]
		op := strings.ToLower(r.Op)
		e := ObjectEvent{Op: op, Txg: r.Txg}

		// Key: full_path is authoritative (SCHEMA.md section 7); NULL
		// full_path = PARTIAL, keep the bare path name, never fabricate.
		switch {
		case r.FullPath != nil:
			e.Key = *r.FullPath
			set.resolved[i] = true
		case r.Path != nil:
			e.Key = *r.Path
		}
		// OldKey (RENAME): old_full_path authoritative; else bare old_path.
		switch {
		case r.OldFullPath != nil:
			e.OldKey = *r.OldFullPath
			set.oldResolved[i] = true
		case r.OldPath != nil:
			e.OldKey = *r.OldPath
		}

		// Sizes: op-constrained columns (SCHEMA.md section 2.1). Only the
		// columns meaningful for this op are read; every other op leaves
		// sizes zero (print_event parity). Present-and-zero is honored
		// (a zero new size is honest, not absent); NULL is never fabricated.
		switch op {
		case "truncate", "setattr":
			if r.Size != nil {
				e.SizeNew = *r.Size
			}
			if r.OldSize != nil {
				e.SizeOld = *r.OldSize
			}
		case "write", "read":
			if r.IoBytes != nil {
				e.SizeNew = int64(*r.IoBytes) //nolint:gosec // G115: io_bytes is a byte count within int64 range on every supported platform
			}
		}

		// Timestamp policy: captured_at > plausibleWallClockTime(hrtime) >
		// zero. row.Timestamp is monotonic hrtime (NOT wall clock), so it
		// only becomes a date via the plausibility guard; never both.
		switch {
		case r.CapturedAt != nil:
			e.Timestamp = time.Unix(*r.CapturedAt, 0).UTC()
		default:
			e.Timestamp = plausibleWallClockTime(r.Timestamp)
		}

		if r.UID != nil {
			e.UID = uint32(*r.UID) //nolint:gosec // G115: uid is 32-bit on every platform zfs events reports; truncation matches zfs behavior (zfs_events.go)
		}
		if r.GID != nil {
			e.GID = uint32(*r.GID) //nolint:gosec // G115: gid is 32-bit on every platform zfs events reports; truncation matches zfs behavior (zfs_events.go)
		}

		set.events = append(set.events, e)
	}
	return set
}

// rowsMatchKey reports whether event i of set is history for the full S3
// key. It is eventMatchesKey over rowEventSet: a resolved row (non-NULL
// full_path) matches on EXACT Key equality only - exactness is a filter,
// never a broadener. A PARTIAL row (NULL full_path) matches conservatively
// via bareMatchesKey: exact bare-name equality, or the queried key ending
// in "/"+bare. OldKey participates with its own resolved/partial rule.
// Showing a possibly-unrelated PARTIAL event beats silently hiding the
// only record of an object (SCHEMA.md section 7, F-live-1).
func rowsMatchKey(set *rowEventSet, i int, key string) bool {
	e := set.events[i]
	if set.resolved[i] {
		if e.Key == key {
			return true
		}
	} else if bareMatchesKey(e.Key, key) {
		return true
	}
	if e.OldKey != "" {
		if set.oldResolved[i] {
			if e.OldKey == key {
				return true
			}
		} else if bareMatchesKey(e.OldKey, key) {
			return true
		}
	}
	return false
}
