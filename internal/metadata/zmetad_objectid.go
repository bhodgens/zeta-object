package metadata

// zmetad_objectid.go — S3 object key -> zmetad object_id resolution for
// the ZFS-native tag store (zfs-metadata#13 consumer side). zmetad keys
// tags on (dataset, object_id) where object_id is the ZFS object id the
// kernel event rows carry; the tag CLI one-shot modes accept ONLY the
// numeric id (zmetad(8): --tag-object "Non-numeric values are rejected"),
// so the path->id mapping lives in the events table.
//
// Resolution rule (SCHEMA.md section 7, layout >= 5): a non-NULL
// full_path is AUTHORITATIVE as of the row's own event and rows carry
// (dataset, txg, id) ordering — so the newest event row whose full_path
// (or old_full_path for a rename) equals the key carries the object's
// CURRENT object_id. That row is the last event on the object's
// now-current name, which is exactly the identity a tag write must
// follow. A RENAME preserves object_id, and a REMOVE deletes both the
// object's event rows' forward identity and its tags in the same ingest
// batch — so "newest row naming this path" cannot resolve a deleted
// object to a live id that still owns tags.
//
// This is an ADDITIVE DB read (SELECT over events), not an interface
// change: the frozen dbHandle/MetadataProvider seams gain nothing.

import (
	"database/sql"
	"fmt"
)

// ResolveObjectID returns the ZFS object id for the dataset-relative
// path (e.g. "docs/report.txt") on dataset. It reads the events table
// directly and returns the object_id of the newest row (highest
// events.id) whose full_path == path or old_full_path == path.
// sql.ErrNoRows maps to *ObjectIDNotFoundError: the dataset has no
// event row naming this path (never seen, fully pruned, or created
// before events were enabled) — the caller must fail the tagging
// request honestly, never guess an id.
func (db *ZmetadDB) ResolveObjectID(dataset, path string) (uint64, error) {
	if dataset == "" || path == "" {
		return 0, &ObjectIDNotFoundError{Dataset: dataset, Path: path}
	}
	var id, objectID uint64
	err := db.conn.QueryRow(
		`SELECT id, object_id FROM events
		 WHERE dataset = ? AND (full_path = ? OR old_full_path = ?)
		 ORDER BY id DESC LIMIT 1`, dataset, path, path).
		Scan(&id, &objectID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, &ObjectIDNotFoundError{Dataset: dataset, Path: path}
		}
		return 0, fmt.Errorf("metadata: resolve object id for %s on %s: %w", path, dataset, err)
	}
	return objectID, nil
}

// ObjectIDNotFoundError means no event row names the path: the object
// id cannot be resolved from the event log.
type ObjectIDNotFoundError struct {
	Dataset string
	Path    string
}

func (e *ObjectIDNotFoundError) Error() string {
	return "metadata: no event row resolves " + e.Path + " on " + e.Dataset + " to an object id"
}
