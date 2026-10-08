// Package metadata provides optional per-bucket metadata enrichment:
// a MetadataProvider seam probed lazily at startup. Providers enrich
// (event history, version listing) but never alter core S3 metadata
// behavior — see docs/plans/metadata-zfs-2026-09/master.md, Contract 5.
package metadata

import (
	"context"
	"time"
)

// MetadataProvider is the frozen seam contract. Implementations MUST be
// safe for concurrent use: Probe/History/Purge may be called from any
// request goroutine.
type MetadataProvider interface {
	Name() string // "zfs-events"
	// Probe: cheap availability check (statfs FS-type + feature check).
	// Called once per bucket at startup.
	Probe(ctx context.Context, bucketPath string) (ProbeResult, error)
	History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error)
	Purge(ctx context.Context, bucketPath string) error
}

type ProbeResult struct {
	Available bool
	Reason    string
	Dataset   string
}

type HistoryQuery struct {
	MaxEvents int
	Since     time.Time
	// SinceID is the event-cursor pass-through (gateway issue #15):
	// only events with a row id STRICTLY GREATER than SinceID are
	// returned. 0 (the zero value) = no cursor, the pre-#15 behavior
	// byte-for-byte. The zmetad provider maps it to WHERE id > ? over
	// the events table's monotonic INTEGER PRIMARY KEY; providers that
	// have no cursor concept ignore it. Struct-field extension is
	// seam-legal: the frozen MetadataProvider interface gains no methods.
	SinceID int64
}

type ObjectEvent struct {
	Op               string // "create"|"remove"|"rename"|"link"|"symlink"|"truncate"|"setattr"
	Key              string
	OldKey           string
	Timestamp        time.Time
	Txg              uint64
	SizeOld, SizeNew int64
	UID, GID         uint32
	// Principal is the opaque application tag carried by ZFS_EV_PRINCIPAL
	// (wire schema 3 / DB layout 8). nil = the writer did not register one
	// (or the record predates the field) - nil is NEVER upgraded to a
	// value. Zero is an honest tag value; the pointer distinguishes
	// present from absent.
	Principal *uint64
	// ID is the zmetad events-table row id (INTEGER PRIMARY KEY
	// AUTOINCREMENT - strictly monotonic per database). This is the
	// event-cursor value (gateway issue #15): the wire form carries it
	// additively as JSON "id" so consumers can resume with since-id=N.
	// 0 = the provider records no cursor id (never fabricated to a
	// value); the wire field is omitted for such events.
	ID int64
}
