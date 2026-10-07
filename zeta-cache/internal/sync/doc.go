// Package sync owns the two-way sync engine (leaf 04): the ETag-diff
// PROPFIND scan with collection-token subtree skip, the locked conflict
// matrix (download / upload-with-If-Match / conflict-copy / delete with
// tombstone / remote-delete-keep-local), the upload and remote-apply
// pipelines, and the ChangeFeed seam leaf 05's event cursor implements
// (default: full scan always). The engine owns the IPC status fields
// (state/lastSync/dirty/conflicts) and exposes SyncOnce(ctx) and
// FullRescan(ctx); the daemon loops are leaf 07's scheduler.
package sync
