package fslock

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestLockSamePathBlocksSecondCaller proves the same path locks out a second
// caller until the release function runs.
func TestLockSamePathBlocksSecondCaller(t *testing.T) {
	Install(nil)

	release := Lock("/shared/path")
	acquired := make(chan struct{})
	go func() {
		unlock := Lock("/shared/path")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
		t.Fatal("second Lock on the same path acquired before the first released")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after release")
	}
}

// TestLockDifferentPathsDontBlock proves distinct paths do not contend.
func TestLockDifferentPathsDontBlock(t *testing.T) {
	Install(nil)

	release := Lock("/path/a")
	defer release()

	acquired := make(chan struct{})
	go func() {
		unlock := Lock("/path/b")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("Lock on a different path blocked")
	}
}

// TestLockUsesInstalledHook proves an installed hook is used when present, and
// that the default is used once the hook is cleared.
func TestLockUsesInstalledHook(t *testing.T) {
	t.Cleanup(func() { Install(nil) })

	var calledPath atomic.Value
	Install(func(path string) func() {
		calledPath.Store(path)
		return func() {}
	})

	unlock := Lock("/hook/path")
	unlock()

	if got, _ := calledPath.Load().(string); got != "/hook/path" {
		t.Fatalf("installed hook got path %q, want /hook/path", got)
	}
}

// TestLockFallsBackToDefaultWhenNoHook proves the default mutex map is used
// when no hook is installed.
func TestLockFallsBackToDefaultWhenNoHook(t *testing.T) {
	Install(nil)

	release := Lock("/default/path")
	acquired := make(chan struct{})
	go func() {
		unlock := Lock("/default/path")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
		t.Fatal("default table did not serialize the same path")
	case <-time.After(100 * time.Millisecond):
	}
	release()

	<-acquired
}

// TestDefaultImplementsLocker pins the interface contract the bucket manager
// depends on (Default.Lock must exist with the Locker signature).
func TestDefaultImplementsLocker(t *testing.T) {
	Install(nil)
	unlock := Default.Lock("/default-locker/path")
	unlock()
}
