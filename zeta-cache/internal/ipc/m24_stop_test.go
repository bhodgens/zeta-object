package ipc

// m24_stop_test.go - the M24 regression: (a) a client that connects and
// never sends must not wedge the accept loop (another client still gets
// served), and (b) Stop must return in bounded time with such a client
// connected (main.go calls Stop BEFORE the bounded-flush unmount).

import (
	"net"
	"testing"
	"time"
)

func TestStopReturnsWithSilentClient(t *testing.T) {
	path := shortSocket(t)
	srv, err := Serve(path, "https://srv", "bkt", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Connect and send NOTHING.
	silent, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	// Give the accept loop a moment to pick the connection up.
	time.Sleep(100 * time.Millisecond)

	// (a) The accept loop is NOT blocked: a well-behaved client is served.
	if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
		t.Fatalf("status behind a silent client: %q", resp.Error)
	}

	// (b) Stop returns within a bounded time despite the silent client.
	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s with a silent client connected")
	}
}

func TestConcurrentSilentClientDoesNotBlockOthers(t *testing.T) {
	path := startServer(t)
	// Three silent clients in a row, then a real request.
	for range 3 {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	time.Sleep(100 * time.Millisecond)
	for range 3 {
		if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
			t.Fatalf("status behind silent clients: %q", resp.Error)
		}
	}
}
