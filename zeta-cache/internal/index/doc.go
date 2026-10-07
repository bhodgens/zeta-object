// Package index owns the client-side SQLite index DB (modernc.org/sqlite,
// no cgo). Leaf 01 opens/creates a PLACEHOLDER database: a real file at the
// configured path with PRAGMA user_version = 0 and no application schema.
// The real schema, WAL mode, resources table, journal, and migrations are
// leaf 03 (index-journal).
package index
