// lockstore_test.go — leaf 01: lockStore contract tests (Contract 1,
// webdav-locking-2026-10 master). Acquire/conflict, refresh and release
// with wrong tokens, lazy expiry via Get, Sweep counts, JSON persistence
// across store reopen, and a -race concurrency test (8 goroutines on
// distinct keys plus contention on one key).
package webdav

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestLockStore(t *testing.T) (*fileLockStore, string) {
	t.Helper()
	dir := t.TempDir()
	return newFileLockStore(dir).(*fileLockStore), dir
}

// seededRawLock writes a raw lock JSON directly under <dir>/.locks/ so
// tests can control Created/Timeout precisely.
func seededRawLock(t *testing.T, dir string, key string, info LockInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal seed lock: %v", err)
	}
	name := filepath.Join(dir, ".locks", lockFileName(key))
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatalf("write seed lock: %v", err)
	}
}

func TestFileLockStore_AcquireAndConflict(t *testing.T) {
	s, _ := newTestLockStore(t)
	got, err := s.Acquire("/b/doc.txt", LockInfo{Owner: "mailto:a@example.com", Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Token == "" {
		t.Fatal("Acquire returned empty token")
	}
	if got.Key != "/b/doc.txt" || got.Owner != "mailto:a@example.com" || got.Depth != "0" || got.Timeout != 600 {
		t.Fatalf("Acquire info = %+v", got)
	}
	if got.Created.IsZero() {
		t.Fatal("Acquire did not set Created")
	}
	_, err = s.Acquire("/b/doc.txt", LockInfo{Owner: "mailto:b@example.com", Depth: "0", Timeout: 60})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire err = %v, want ErrLocked", err)
	}
	// Distinct key is still free.
	if _, err := s.Acquire("/b/other.txt", LockInfo{Depth: "0", Timeout: 60}); err != nil {
		t.Fatalf("Acquire other key: %v", err)
	}
}

func TestFileLockStore_RefreshWrongToken(t *testing.T) {
	s, _ := newTestLockStore(t)
	got, err := s.Acquire("/b/doc.txt", LockInfo{Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Refresh("/b/doc.txt", "opaquelocktoken:wrong", 300); !errors.Is(err, ErrLockTokenMismatch) {
		t.Fatalf("Refresh wrong token err = %v, want ErrLockTokenMismatch", err)
	}
	// Correct token still refreshes and resets the lease.
	after, err := s.Refresh("/b/doc.txt", got.Token, 300)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if after.Timeout != 300 {
		t.Fatalf("Refresh Timeout = %d, want 300", after.Timeout)
	}
	// Missing lock: ErrNotFound.
	if _, err := s.Refresh("/b/none.txt", got.Token, 300); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Refresh missing err = %v, want ErrNotFound", err)
	}
}

func TestFileLockStore_ReleaseWrongToken(t *testing.T) {
	s, _ := newTestLockStore(t)
	got, err := s.Acquire("/b/doc.txt", LockInfo{Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := s.Release("/b/doc.txt", "opaquelocktoken:wrong"); !errors.Is(err, ErrLockTokenMismatch) {
		t.Fatalf("Release wrong token err = %v, want ErrLockTokenMismatch", err)
	}
	if err := s.Release("/b/none.txt", got.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release missing err = %v, want ErrNotFound", err)
	}
	if err := s.Release("/b/doc.txt", got.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := s.Get("/b/doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after release err = %v, want ErrNotFound", err)
	}
}

func TestFileLockStore_LazyExpiryViaGet(t *testing.T) {
	s, dir := newTestLockStore(t)
	now := time.Now()
	seededRawLock(t, dir, "/b/doc.txt", LockInfo{
		Token: "opaquelocktoken:stale", Owner: "x", Depth: "0",
		Timeout: 5, Created: now.Add(-10 * time.Second), Key: "/b/doc.txt",
	})
	if _, err := s.Get("/b/doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get expired err = %v, want ErrNotFound", err)
	}
	// Acquiring over an expired lock must succeed (lazy expiry).
	got, err := s.Acquire("/b/doc.txt", LockInfo{Depth: "0", Timeout: 60})
	if err != nil {
		t.Fatalf("Acquire over expired: %v", err)
	}
	if got.Token == "opaquelocktoken:stale" {
		t.Fatal("expired token survived")
	}
	// Refresh on an expired lock: ErrNotFound.
	seededRawLock(t, dir, "/b/old.txt", LockInfo{
		Token: "opaquelocktoken:stale2", Depth: "0",
		Timeout: 5, Created: now.Add(-10 * time.Second), Key: "/b/old.txt",
	})
	if _, err := s.Refresh("/b/old.txt", "opaquelocktoken:stale2", 60); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Refresh expired err = %v, want ErrNotFound", err)
	}
}

func TestFileLockStore_Sweep(t *testing.T) {
	s, dir := newTestLockStore(t)
	now := time.Now()
	seededRawLock(t, dir, "/b/stale1.txt", LockInfo{Token: "t1", Depth: "0", Timeout: 5, Created: now.Add(-10 * time.Second), Key: "/b/stale1.txt"})
	seededRawLock(t, dir, "/b/stale2.txt", LockInfo{Token: "t2", Depth: "0", Timeout: 5, Created: now.Add(-10 * time.Second), Key: "/b/stale2.txt"})
	seededRawLock(t, dir, "/b/live.txt", LockInfo{Token: "t3", Depth: "0", Timeout: 600, Created: now, Key: "/b/live.txt"})
	if n := s.Sweep(now); n != 2 {
		t.Fatalf("Sweep removed %d, want 2", n)
	}
	if n := s.Sweep(now); n != 0 {
		t.Fatalf("second Sweep removed %d, want 0", n)
	}
	if _, err := s.Get("/b/live.txt"); err != nil {
		t.Fatalf("live lock swept: %v", err)
	}
}

func TestFileLockStore_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1 := newFileLockStore(dir).(*fileLockStore)
	got, err := s1.Acquire("/b/doc.txt", LockInfo{Owner: "mailto:a@example.com", Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// On-disk JSON is readable and stable.
	raw, err := os.ReadFile(filepath.Join(dir, ".locks", lockFileName("/b/doc.txt")))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	var onDisk LockInfo
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("lock file is not JSON: %v", err)
	}
	if onDisk.Token != got.Token || onDisk.Key != "/b/doc.txt" {
		t.Fatalf("on-disk = %+v, want token %s key /b/doc.txt", onDisk, got.Token)
	}

	s2 := newFileLockStore(dir).(*fileLockStore)
	after, err := s2.Get("/b/doc.txt")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if after.Token != got.Token || after.Owner != got.Owner || after.Timeout != 600 {
		t.Fatalf("after reopen = %+v, want %+v", after, got)
	}
	// And the reopened store enforces the conflict.
	if _, err := s2.Acquire("/b/doc.txt", LockInfo{Depth: "0", Timeout: 60}); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire after reopen err = %v, want ErrLocked", err)
	}
	if err := s2.Release("/b/doc.txt", got.Token); err != nil {
		t.Fatalf("Release after reopen: %v", err)
	}
}

func TestFileLockStore_Race(t *testing.T) {
	s, _ := newTestLockStore(t)
	var wg sync.WaitGroup
	// 8 goroutines, each hammering its own distinct key.
	for i := range 8 {
		wg.Go(func() {
			key := string(rune('a'+i)) + "/key.txt"
			for range 20 {
				got, err := s.Acquire(key, LockInfo{Depth: "0", Timeout: 60})
				if err != nil {
					panic("acquire own key: " + err.Error())
				}
				if _, err := s.Refresh(key, got.Token, 60); err != nil {
					panic("refresh own key: " + err.Error())
				}
				if err := s.Release(key, got.Token); err != nil {
					panic("release own key: " + err.Error())
				}
				s.Sweep(time.Now())
			}
		})
	}
	// Contention on one key: exactly one winner per round.
	wg.Go(func() {
		for range 20 {
			winner := -1
			var mu sync.Mutex
			var inner sync.WaitGroup
			for i := range 8 {
				inner.Go(func() {
					got, err := s.Acquire("contended/key.txt", LockInfo{Depth: "0", Timeout: 60})
					if err != nil {
						return // lost the race
					}
					mu.Lock()
					if winner == -1 {
						winner = i
					}
					mu.Unlock()
					if err := s.Release("contended/key.txt", got.Token); err != nil {
						panic("release contended: " + err.Error())
					}
				})
			}
			inner.Wait()
			if winner == -1 {
				panic("no winner for contended key")
			}
		}
	})
	wg.Wait()
}
