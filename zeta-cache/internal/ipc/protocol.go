package ipc

// protocol.go - the leaf-08 protocol additions: conflict / recently-
// deleted / evict requests. Everything here is ADDITIVE to the leaf-01/07
// wire shapes: the Request gains two optional fields, responses ride the
// existing Extra field, and the envelope {v, ok, data|error} is untouched.
// A daemon whose handler does not implement ConflictResolver answers the
// new types with a capability error (the same rule leaf 07 uses for a nil
// Handler).

// Conflict item kinds (conflicts.list entries).
const (
	// KindConflictCopy: the matrix-3 conflict: the remote generation was
	// applied to path and the local bytes were preserved under CopyPath.
	KindConflictCopy = "conflict-copy"
	// KindKeptLocal: the matrix-5 conflict: the remote deleted the file
	// but it had local edits, which were kept (dirty) locally.
	KindKeptLocal = "kept-local"
)

// Conflict resolution modes (conflicts.resolve).
const (
	ModeKeepLocal  = "keep-local"  // the local copy wins (uploaded)
	ModeKeepRemote = "keep-remote" // the remote copy wins (downloaded over the local)
)

// ConflictItem is one entry of the conflicts.list payload.
type ConflictItem struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // KindConflictCopy | KindKeptLocal
	// CopyPath is the preserved conflicted-copy file (empty for
	// KindKeptLocal, which has no copy).
	CopyPath string `json:"copyPath,omitempty"`
}

// ConflictData is the conflicts.list payload.
type ConflictData struct {
	Conflicts []ConflictItem `json:"conflicts"`
}

// ResolveData is the conflicts.resolve payload.
type ResolveData struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	// CopyRemoved reports whether the conflicted-copy file was removed
	// (only ever true when the request carried confirm=true).
	CopyRemoved bool `json:"copyRemoved"`
}

// RestoreData is the deleted.restore payload.
type RestoreData struct {
	Path string `json:"path"`
}

// EvictData is the evict payload.
type EvictData struct {
	Path string `json:"path"`
}

// ConflictResolver is the additive capability seam for the leaf-08
// requests. A Handler may ALSO implement this interface; the server type-
// asserts per request and answers the capability error when it does not.
// All methods must be safe for concurrent use.
type ConflictResolver interface {
	// DeletedPaths lists the tombstoned paths (deleted.list).
	DeletedPaths() ([]Tombstone, error)
	// Conflicts lists the pending conflict records.
	Conflicts() ([]ConflictItem, error)
	// ResolveConflict applies mode ("keep-local" | "keep-remote") to
	// path. removeCopy additionally deletes the conflicted-copy FILE
	// (never done implicitly).
	ResolveConflict(path, mode string, removeCopy bool) error
	// RestoreDeleted re-downloads a tombstoned path if the server still
	// serves it; an unrecoverable path is an error, never a silent ok.
	RestoreDeleted(path string) error
	// EvictPath evicts ONE clean file now; dirty/pinned/unknown paths
	// are refused with an error.
	EvictPath(path string) error
}

// resolveModeValid reports whether mode is one of the two locked modes.
func resolveModeValid(mode string) bool {
	return mode == ModeKeepLocal || mode == ModeKeepRemote
}
