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
