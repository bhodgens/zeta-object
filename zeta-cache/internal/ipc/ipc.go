package ipc

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Protocol version carried by every message.
const Version = 1

// Request is the wire shape of every IPC request. Type selects the handler;
// v must equal Version.
type Request struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	Net  string `json:"net,omitempty"` // reserved for leaf 08
	Path string `json:"path,omitempty"`
	Pin  *bool  `json:"pin,omitempty"`
	// Leaf-08 additions (additive optional fields; leaf-01/07 clients
	// never set them and unaffected handlers ignore them):
	// Mode selects the conflicts.resolve resolution.
	Mode string `json:"mode,omitempty"`
	// Confirm is the explicit user flag: only a resolve request carrying
	// confirm=true may DELETE the conflicted-copy file.
	Confirm bool `json:"confirm,omitempty"`
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
	// Leaf 07 additions (additive fields; leaf-01 clients tolerate them
	// because they only read the fields they know).
	Paused      bool   `json:"paused"`
	UsageBytes  int64  `json:"usageBytes"`
	CapBytes    int64  `json:"capBytes"`
	Overflow    int    `json:"overflow"`
	Evicted     int64  `json:"evicted"`
	LastEvictAt any    `json:"lastEvictAt"`
	LastErr     string `json:"lastError,omitempty"`
}

// TombstoneData is the payload of the tombstones response (the GUI
// restore view's input; the restore itself is leaf 08).
type TombstoneData struct {
	Tombstones []Tombstone `json:"tombstones"`
}

// Tombstone is one deletion-grace row.
type Tombstone struct {
	Path      string `json:"path"`
	DeletedAt int64  `json:"deletedAt"` // unix seconds
	ExpiresAt int64  `json:"expiresAt"` // unix seconds (DeletedAt + retention)
}

// PinData is the payload of the pins response.
type PinData struct {
	Pins []string `json:"pins"`
}

// Response is the wire shape of every IPC response.
type Response struct {
	V     int         `json:"v"`
	OK    bool        `json:"ok"`
	Data  *StatusData `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
	// Extra carries the non-status payloads (tombstones/pins). It is a
	// second optional field rather than a type union so the leaf-01
	// status shape is untouched.
	Extra any `json:"extra,omitempty"`
}

// ServerState is what status reports in data.state when no live status
// source is wired (leaf-01 static behavior).
const ServerState = "idle"

// Handler is the seam the daemon (leaf 07's scheduler wiring, leaf 08's
// GUI) implements to serve the state-changing and query requests. A nil
// field means the request answers with a capability error instead.
type Handler interface {
	// Pause suspends the scheduler loops; Resume restarts them. Both
	// return the post-transition paused state.
	Pause() (bool, error)
	Resume() (bool, error)
	// SetPin toggles the pinned flag for path (hydration on pin is the
	// daemon's job).
	SetPin(path string, pinned bool) error
	// Pins lists the pinned paths.
	Pins() ([]string, error)
	// Tombstones lists the deletion-grace rows (path + window).
	Tombstones() ([]Tombstone, error)
}

// DeletedLister is the additive capability behind deleted.list (the
// tombstones LIST already exists via Handler; the GUI's recently-deleted
// view uses deleted.list so the request names match the response shape).
type DeletedLister interface {
	// DeletedPaths lists the tombstoned rows (path + deletion window).
	DeletedPaths() ([]Tombstone, error)
}

// StatusSource supplies the live status values for the status response.
// Implementations must be safe for concurrent use and must not block on
// the network.
type StatusSource interface {
	// IPCStatus returns the current values.
	IPCStatus() StatusData
}

// Server is a running IPC listener. Stop closes it and removes the socket
// file.
type Server struct {
	ln         net.Listener
	socketPath string
	mu         sync.Mutex // guards status+handler: ReplaceHandler swaps them live
	status     StatusSource
	handler    Handler
	wg         *sync.WaitGroup
}

// ReplaceHandler swaps the status source and handler on the LIVE listener:
// the socket file, its 0600 mode, and connected clients are untouched.
// Used by main.go's two-phase startup (leaf-01 shape answers first, the
// full GUI surface replaces it once the engine/scheduler exist) without
// the teardown window that deleted the live socket.
func (s *Server) ReplaceHandler(status StatusSource, handler Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.handler = handler
}

// Serve listens on socketPath (0600, stale socket file removed first) and
// answers one JSON request per connection until Stop is called. handler
// may be nil (leaf-01 behavior: only status answers; state-changing
// requests get a capability error).
func Serve(socketPath string, serverURL, bucket string, status StatusSource) (*Server, error) {
	return ServeWithHandler(socketPath, serverURL, bucket, status, nil)
}

// ServeWithHandler is Serve plus the state-changing seam.
func ServeWithHandler(socketPath string, serverURL, bucket string, status StatusSource, handler Handler) (*Server, error) {
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
	srv := &Server{ln: ln, socketPath: socketPath, status: status, handler: handler}
	var wg sync.WaitGroup
	srv.wg = &wg
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: Stop was called
			}
			// Read status/handler under the server mutex: main.go's
			// two-phase startup swaps in the full GUI handler on this
			// live listener (ReplaceHandler) without socket teardown.
			srv.mu.Lock()
			st, h := srv.status, srv.handler
			srv.mu.Unlock()
			handleConn(conn, serverURL, bucket, st, h)
		}
	})
	return srv, nil
}

// wg tracks the accept loop so Stop waits for in-flight connections to
// finish their response write instead of cutting them mid-write. (The
// WaitGroup is populated in Serve; Stop's close happens-before the
// Accept error return, so a concurrent Stop never misses the loop.)

// Stop closes the listener and removes the socket file. Safe to call more
// than once. In-flight connections are closed with the listener, which is
// the documented restart semantics: clients of a stopping daemon fail on
// their socket instead of getting answers from a half-closed daemon.
func (s *Server) Stop() {
	_ = s.ln.Close()
	if s.wg != nil {
		s.wg.Wait()
	}
	_ = os.Remove(s.socketPath)
}

func handleConn(conn net.Conn, serverURL, bucket string, status StatusSource, handler Handler) {
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
	case "pause", "resume":
		if handler == nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: "not supported: no scheduler handler"})
			return
		}
		var (
			paused bool
			err    error
		)
		if req.Type == "pause" {
			paused, err = handler.Pause()
		} else {
			paused, err = handler.Resume()
		}
		if err != nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: err.Error()})
			return
		}
		data := statusOrDefault(status, serverURL, bucket)
		data.Paused = paused
		writeResponse(conn, Response{V: Version, OK: true, Data: &data})
	case "pin":
		if handler == nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: "not supported: no scheduler handler"})
			return
		}
		if req.Path == "" || req.Pin == nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: "pin requires path and pin fields"})
			return
		}
		if err := handler.SetPin(req.Path, *req.Pin); err != nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: err.Error()})
			return
		}
		pins, err := handler.Pins()
		if err != nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{V: Version, OK: true, Extra: PinData{Pins: pins}})
	case "pins":
		if handler == nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: "not supported: no scheduler handler"})
			return
		}
		pins, err := handler.Pins()
		if err != nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{V: Version, OK: true, Extra: PinData{Pins: pins}})
	case "tombstones":
		if handler == nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: "not supported: no scheduler handler"})
			return
		}
		tombs, err := handler.Tombstones()
		if err != nil {
			writeResponse(conn, Response{V: Version, OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{V: Version, OK: true, Extra: TombstoneData{Tombstones: tombs}})
	default:
		// Leaf-08 types dispatch through the additive ConflictResolver
		// capability: a handler that does not implement it (and no
		// handler at all) answers the capability error, keeping the
		// unknown-type rejection and the leaf-01/07 surfaces unchanged.
		if req.Type == "conflicts.list" || req.Type == "conflicts.resolve" ||
			req.Type == "deleted.list" || req.Type == "deleted.restore" || req.Type == "evict" {
			writeResponse(conn, handleLeaf08(req, handler))
			return
		}
		writeResponse(conn, Response{V: Version, OK: false, Error: "unknown request type"})
	}
}

// handleLeaf08 serves the leaf-08 request types against the additive
// ConflictResolver capability. handler may be nil or lack the capability;
// both answer the capability error.
func handleLeaf08(req Request, handler Handler) Response {
	resolver, ok := handler.(ConflictResolver)
	if handler == nil || !ok {
		return Response{V: Version, OK: false, Error: "not supported: no conflict handler"}
	}
	switch req.Type {
	case "conflicts.list":
		items, err := resolver.Conflicts()
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: ConflictData{Conflicts: items}}
	case "conflicts.resolve":
		if req.Path == "" || !resolveModeValid(req.Mode) {
			return Response{V: Version, OK: false, Error: "conflicts.resolve requires path and mode (keep-local|keep-remote)"}
		}
		if err := resolver.ResolveConflict(req.Path, req.Mode, req.Confirm); err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: ResolveData{
			Path: req.Path, Mode: req.Mode, CopyRemoved: req.Confirm,
		}}
	case "deleted.list":
		tombs, err := resolver.DeletedPaths()
		if err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: TombstoneData{Tombstones: tombs}}
	case "deleted.restore":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "deleted.restore requires path"}
		}
		if err := resolver.RestoreDeleted(req.Path); err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: RestoreData{Path: req.Path}}
	case "evict":
		if req.Path == "" {
			return Response{V: Version, OK: false, Error: "evict requires path"}
		}
		if err := resolver.EvictPath(req.Path); err != nil {
			return Response{V: Version, OK: false, Error: err.Error()}
		}
		return Response{V: Version, OK: true, Extra: EvictData{Path: req.Path}}
	}
	// Unreachable: the caller only routes the five leaf-08 types here.
	return Response{V: Version, OK: false, Error: "unknown request type"}
}

func statusOrDefault(status StatusSource, serverURL, bucket string) StatusData {
	data := StatusData{State: ServerState, Server: serverURL, Bucket: bucket}
	if status != nil {
		data = status.IPCStatus()
		data.Server = serverURL
		data.Bucket = bucket
	}
	return data
}

func writeResponse(conn net.Conn, resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = conn.Write(data)
}
