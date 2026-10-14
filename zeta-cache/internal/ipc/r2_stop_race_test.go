package ipc

// r2_stop_race_test.go - the R2 regression: the accept loop used to
// register each connection's goroutine in the WaitGroup AFTER Accept
// returned, while Stop snapshotted s.conns after ln.Close(). A conn
// dequeued just before Close was tracked after the snapshot, and its
// goroutine registered with the WaitGroup after Wait - Stop could then
// block up to the 5-minute idleReadDeadline on a connect-and-never-send
// client. The fix tracks and deadline-arms in the accept loop, and a
// stopped flag force-closes any conn tracked in the residual window.
//
// The test hammers the window: N silent clients dial immediately, Stop
// must return well under the 5-minute deadline.

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestR2StopReturnsWithManySilentClients(t *testing.T) {
	path := shortSocket(t)
	srv, err := Serve(path, "https://srv", "bkt", nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	conns := make([]net.Conn, 0, n)
	for range n {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	// Do NOT sleep: dialing straight into Stop is what hits the
	// accept-loop/Stop race window.

	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s with silent clients connected (Stop race)")
	}
}

// Repeat the tight dial-then-Stop sequence several times to make the
// narrow window (conn accepted just before ln.Close) probable.
func TestR2StopRaceRepeated(t *testing.T) {
	for i := range 5 {
		path := shortSocket(t)
		srv, err := Serve(path, "https://srv", "bkt", nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			srv.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.Close()
			t.Fatalf("iteration %d: Stop wedged past 2s", i)
		}
		_ = c.Close()
	}
}

// TestR2StopWithPostAcceptPause pins the Accept->trackConn window
// DETERMINISTICALLY. The racy dial-then-Stop tests above only make the
// window probable; the testTrackDelay hook (test-only, nil in
// production) parks the accept loop AFTER Accept returned and BEFORE
// trackConn - a client dialing into that pause is accepted, then Stop
// runs while the accept loop sits in the window. On the pre-fix code
// (track inside the per-conn goroutine, snapshot after ln.Close) that
// conn is registered with the WaitGroup after Wait, its deadline never
// armed, and Stop blocks toward the 5-minute idle deadline. The
// contract: Stop still returns promptly and the stray conn is
// force-closed by the stopped flag.
func TestR2StopWithPostAcceptPause(t *testing.T) {
	path := shortSocket(t)
	srv, err := Serve(path, "https://srv", "bkt", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Park every trackConn until we release it - the deterministic
	// stand-in for "conn dequeued just before ln.Close()".
	var (
		mu       sync.Mutex
		released bool
		parked   = make(chan struct{})
	)
	srv.mu.Lock()
	srv.testTrackDelay = func() {
		mu.Lock()
		done := released
		mu.Unlock()
		if done {
			return
		}
		parked <- struct{}{} // signal: parked in the window (buffered below)
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			d := released
			mu.Unlock()
			if d || time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	srv.mu.Unlock()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Wait until the accept loop has parked in the window, then release
	// it INTO the Stop sequence.
	<-parked
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	released = true
	mu.Unlock()

	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s with a conn parked in the Accept->trackConn window")
	}
	// The stray conn must be force-closed by the stopped flag: reads on
	// it fail rather than hang toward the 5-minute deadline.
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(buf); err == nil {
		t.Fatal("stray conn still open after Stop (stopped flag did not force-close it)")
	}
}
