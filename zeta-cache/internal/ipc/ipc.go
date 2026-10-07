package ipc

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

// Protocol version carried by every message.
const Version = 1

// Request is the wire shape of every IPC request. Type selects the handler;
// v must equal Version.
type Request struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	Net  string `json:"net,omitempty"` // reserved for leaf 08
}

// StatusData is the payload of a successful status response.
type StatusData struct {
	State     string `json:"state"`
	Server    string `json:"server"`
	Bucket    string `json:"bucket"`
	LastSync  any    `json:"lastSync"`
	Dirty     int    `json:"dirty"`
	Cached    int    `json:"cached"`
	Conflicts int    `json:"conflicts"`
}

// Response is the wire shape of every IPC response.
type Response struct {
	V     int         `json:"v"`
	OK    bool        `json:"ok"`
	Data  *StatusData `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
}

// ServerState is what status reports in data.state. Leaf 01 only ever
// produces "idle"; leaves 04/07 introduce syncing/error transitions.
const ServerState = "idle"

// StatusSource supplies the live status values for the status response.
// Leaf 04 wires the sync engine through it (state/lastSync/dirty/
// conflicts); a nil source keeps the leaf-01 static behavior (state
// "idle", zero counters) - the protocol shape is unchanged.
type StatusSource interface {
	// IPCStatus returns the current values. Implementations must be
	// safe for concurrent use and must not block on the network.
	IPCStatus() StatusData
}

// Server is a running IPC listener. Stop closes it and removes the socket
// file.
type Server struct {
	ln         net.Listener
	socketPath string
	status     StatusSource
}

// Serve listens on socketPath (0600, stale socket file removed first) and
// answers one JSON request per connection until Stop is called.
func Serve(socketPath string, serverURL, bucket string, status StatusSource) (*Server, error) {
	if dir := filepath.Dir(socketPath); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("ipc: creating %s: %w", dir, err)
		}
	}
	// A previous daemon that died hard leaves the socket file behind;
	// bind would fail with EADDRINUSE, so remove the stale file first.
	// A live daemon's socket file is also removed here, which is the
	// documented restart semantics: the new daemon takes over the path and
	// the old daemon's clients fail on their next dial/connect (their
	// connection was to the old, now-orphaned socket inode).
	if fi, err := os.Lstat(socketPath); err == nil {
		if fi.Mode()&os.ModeSocket != 0 {
			log.Printf("zeta-cache: WARNING removing stale IPC socket %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("ipc: removing stale socket %s: %w", socketPath, err)
		}
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("ipc: listening on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("ipc: chmod %s: %w", socketPath, err)
	}
	srv := &Server{ln: ln, socketPath: socketPath, status: status}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: Stop was called
			}
			handleConn(conn, serverURL, bucket, status)
		}
	}()
	return srv, nil
}

// Stop closes the listener and removes the socket file. Safe to call more
// than once. In-flight connections are closed with the listener, which is
// the documented restart semantics: clients of a stopping daemon fail on
// their socket instead of getting answers from a half-closed daemon.
func (s *Server) Stop() {
	_ = s.ln.Close()
	_ = os.Remove(s.socketPath)
}

func handleConn(conn net.Conn, serverURL, bucket string, status StatusSource) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		writeResponse(conn, Response{V: Version, OK: false, Error: "bad request: " + err.Error()})
		return
	}
	if req.V != Version {
		writeResponse(conn, Response{V: Version, OK: false, Error: fmt.Sprintf("unsupported protocol version %d (want %d)", req.V, Version)})
		return
	}
	switch req.Type {
	case "status":
		data := StatusData{
			State:  ServerState,
			Server: serverURL,
			Bucket: bucket,
		}
		if status != nil {
			live := status.IPCStatus()
			live.Server = serverURL // identity fields are the listener's
			live.Bucket = bucket
			data = live
		}
		writeResponse(conn, Response{V: Version, OK: true, Data: &data})
	default:
		writeResponse(conn, Response{V: Version, OK: false, Error: "unknown request type"})
	}
}

func writeResponse(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = conn.Write(data)
}
