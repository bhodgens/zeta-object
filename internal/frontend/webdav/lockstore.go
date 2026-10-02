// lockstore.go — leaf 01 (webdav-locking-2026-10): the lockStore seam
// (Contract 1) and the file-backed implementation. One JSON file per lock
// under <dir>/.locks/<key>.lock; lazy expiry on access plus an explicit
// Sweep for interval callers. Charter note: .metadata/.locks/ is runtime
// coordination state under the bucket's existing metadata area (like the
// uploads staging area), NOT per-object identity metadata.
package webdav

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LockInfo describes one WebDAV lock (Contract 1). Token uses the
// opaquelocktoken:UUID scheme; UUID bytes come from crypto/rand (no
// external uuid dependency).
type LockInfo struct {
	Token   string    // opaquelocktoken:UUID
	Owner   string    // href from the LOCK body (may be empty)
	Depth   string    // "0" (only depth supported in v1)
	Timeout int64     // seconds remaining at LastRefresh
	Created time.Time
	Key     string // resource path, slash-led, no trailing slash
}

// Typed lock errors; the handler layer maps them onto 423/409/404.
var (
	ErrLocked            = errors.New("webdav: resource is locked")
	ErrLockTokenMismatch = errors.New("webdav: lock token does not match")
	ErrNotFound          = errors.New("webdav: lock not found")
)

// lockStore is the lock coordination seam (Contract 1). Implementations
// must be safe for concurrent use.
type lockStore interface {
	Acquire(key string, info LockInfo) (LockInfo, error) // ErrLocked if held
	Refresh(key, token string, timeout int64) (LockInfo, error)
	Release(key, token string) error // wrong token: ErrLockTokenMismatch
	Get(key string) (LockInfo, error)
	Sweep(now time.Time) int // remove expired, return count
}

// newLockErr / newLockMismatch wrap the typed errors with context while
// staying errors.Is-identifiable.
func newLockErr(base error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{base}, args...)...)
}

// lockFileName maps a resource key onto the on-disk lock file name. The
// key is slash-led; slashes become underscores after stripping the lead so
// every key maps to a single flat file name (depth-1 keys only — that is
// all v1 issues LOCKs for). Keys arrive from parseResource, which rejects
// dot-segment traversal, and the underscore flattening leaves no path
// structure for an escaped segment to exploit.
func lockFileName(key string) string {
	return strings.ReplaceAll(strings.TrimPrefix(key, "/"), "/", "_") + ".lock"
}

// fileLockStore stores one JSON file per lock under <dir>/.locks/. A
// per-store mutex serializes the rename/rename race on the same key file;
// the lock files themselves are the persistence format.
type fileLockStore struct {
	dir   string // <dir>/.locks
	mu    sync.Mutex
	nowFn func() time.Time // injectable clock for tests
}

// newFileLockStore returns a file-backed lockStore rooted under
// <dir>/.locks/ (Contract 1). The directory is created lazily on first
// use but eagerly here so a missing root fails fast.
func newFileLockStore(dir string) lockStore {
	root := filepath.Join(dir, ".locks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		// Surface a store that errors on use rather than panicking here;
		// the constructor signature is error-free per Contract 1.
		return &fileLockStore{dir: root, nowFn: time.Now}
	}
	return &fileLockStore{dir: root, nowFn: time.Now}
}

func (s *fileLockStore) path(key string) string {
	return filepath.Join(s.dir, lockFileName(key))
}

// expired reports whether the lock's lease has run out relative to now.
func expired(info LockInfo, now time.Time) bool {
	return now.Sub(info.Created) >= time.Duration(info.Timeout)*time.Second
}

// readRaw loads the lock file without expiry filtering.
func (s *fileLockStore) readRaw(key string) (LockInfo, error) {
	data, err := os.ReadFile(s.path(key)) //nolint:gosec // G703: key is parseResource-vetted, flattened by lockFileName
	if errors.Is(err, os.ErrNotExist) {
		return LockInfo{}, ErrNotFound
	}
	if err != nil {
		return LockInfo{}, newLockErr(ErrNotFound, "read lock %s: %v", key, err)
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return LockInfo{}, newLockErr(ErrNotFound, "parse lock %s: %v", key, err)
	}
	info.Key = key
	return info, nil
}

// readLive loads the lock and applies lazy expiry: an expired lock is
// removed from disk and reported as ErrNotFound.
func (s *fileLockStore) readLive(key string) (LockInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLiveLocked(key)
}

func (s *fileLockStore) readLiveLocked(key string) (LockInfo, error) {
	info, err := s.readRaw(key)
	if err != nil {
		return LockInfo{}, err
	}
	if expired(info, s.nowFn()) {
		// Best-effort unlink: a failed remove (ENOENT from a concurrent
		// sweeper, EPERM) must not mask the ErrNotFound the caller acts
		// on; the next access or Sweep retries the removal.
		if rmErr := os.Remove(s.path(key)); rmErr != nil && !os.IsNotExist(rmErr) { //nolint:gosec // G703: key is parseResource-vetted, flattened by lockFileName
			// Surface unusual failures in logs without changing behavior.
			fmt.Fprintf(os.Stderr, "webdav: lockstore remove %s: %v\n", s.path(key), rmErr)
		}
		return LockInfo{}, ErrNotFound
	}
	return info, nil
}

// writeLocked persists info under the store mutex.
func (s *fileLockStore) writeLocked(info LockInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("webdav: marshal lock %s: %w", info.Key, err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil { //nolint:gosec // G703: dir is the bucket .metadata root, not request-derived
		return fmt.Errorf("webdav: lock dir: %w", err)
	}
	tmp := s.path(info.Key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G703: key is parseResource-vetted, flattened by lockFileName
		return fmt.Errorf("webdav: write lock %s: %w", info.Key, err)
	}
	if err := os.Rename(tmp, s.path(info.Key)); err != nil { //nolint:gosec // G703: key is parseResource-vetted, flattened by lockFileName
		return fmt.Errorf("webdav: commit lock %s: %w", info.Key, err)
	}
	return nil
}

// newLockToken returns an "opaquelocktoken:" URI with a crypto/rand UUID
// v4 (hex encoding, no external dependency).
func newLockToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("webdav: lock token: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return "opaquelocktoken:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func (s *fileLockStore) Acquire(key string, info LockInfo) (LockInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.readLiveLocked(key); err == nil {
		return LockInfo{}, newLockErr(ErrLocked, "%s is already locked", key)
	}
	token, err := newLockToken()
	if err != nil {
		return LockInfo{}, err
	}
	got := info
	got.Token = token
	got.Key = key
	got.Created = s.nowFn().UTC()
	if err := s.writeLocked(got); err != nil {
		return LockInfo{}, err
	}
	return got, nil
}

func (s *fileLockStore) Refresh(key, token string, timeout int64) (LockInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.readLiveLocked(key)
	if err != nil {
		return LockInfo{}, err
	}
	if info.Token != token {
		return LockInfo{}, newLockErr(ErrLockTokenMismatch, "%s held by another token", key)
	}
	info.Timeout = timeout
	info.Created = s.nowFn().UTC()
	if err := s.writeLocked(info); err != nil {
		return LockInfo{}, err
	}
	return info, nil
}

func (s *fileLockStore) Release(key, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.readLiveLocked(key)
	if err != nil {
		return err
	}
	if info.Token != token {
		return newLockErr(ErrLockTokenMismatch, "%s held by another token", key)
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: key is parseResource-vetted, flattened by lockFileName
		return fmt.Errorf("webdav: release lock %s: %w", key, err)
	}
	return nil
}

func (s *fileLockStore) Get(key string) (LockInfo, error) {
	return s.readLive(key)
}

// Sweep removes every expired lock file and returns the count removed.
func (s *fileLockStore) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var info LockInfo
		if json.Unmarshal(data, &info) != nil || expired(info, now) {
			if os.Remove(filepath.Join(s.dir, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}
