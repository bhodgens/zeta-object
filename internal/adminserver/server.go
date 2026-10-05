package adminserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
)

// server.go — the console skeleton: HTTP surface assembly, the operator
// sign-in exchange, and the session/CSRF gate in front of every /api/ path.
// This leaf owns the skeleton; the proxy routes themselves are leaf 05 and
// register through HandleAPI.

// errorEnvelope is the console's JSON error shape: an object with a top-level
// "error" key holding string "code" and "message" fields (Contract 2).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Server is the console HTTP server skeleton.
type Server struct {
	cfg      *Config
	sessions *sessionStore
	static   *staticHandler

	mu  sync.Mutex
	api *http.ServeMux
}

// NewServer builds a Server from a validated config. It fails only if the
// per-process session key cannot be generated.
func NewServer(cfg *Config) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("adminserver: nil config")
	}
	store, err := newSessionStore()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		sessions: store,
		static:   newStaticHandler(webFS),
		api:      http.NewServeMux(),
	}, nil
}

// Handler returns the console's root HTTP handler.
func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	root.Handle("/", s.static)
	root.HandleFunc("/login", s.handleLogin)
	root.HandleFunc("/logout", s.handleLogout)
	// Every /api/ path passes through the session gate; the concrete routes
	// are registered later via HandleAPI.
	root.Handle("/api/", s.sessionGate(s.api))
	return root
}

// HandleAPI registers an /api/ route behind the session gate. path is the
// full console path (for example "/api/status"); method is the single allowed
// method. Leaf 05 registers the proxy handlers here.
func (s *Server) HandleAPI(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.api.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		h(w, r)
	})
}

// sessionGate requires a valid session on every request and, for mutating
// requests, a matching CSRF header. Failures use the JSON error envelope.
func (s *Server) sessionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		csrf, ok := s.sessions.validate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid session is required")
			return
		}
		if isMutating(r.Method) {
			if !tokenEqual(r.Header.Get(CSRFHeaderName), csrf) {
				writeError(w, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin serves the sign-in page (GET) and exchanges the operator token
// for a session (POST).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.static.serveLogin(w, r)
	case http.MethodPost:
		s.login(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// login verifies the presented operator token in constant time and mints a
// session. The token and the derived session secret are never logged.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid sign-in request")
		return
	}
	if !tokenEqual(body.Token, s.cfg.OperatorToken) {
		writeError(w, http.StatusUnauthorized, "invalid_token", "operator token is not valid")
		return
	}
	csrf, err := s.sessions.mint(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not start a session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": csrf})
}

// handleLogout clears the session and its cookies.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.sessions.clear(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// isMutating reports whether a method changes state and so needs CSRF.
func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// writeError writes the JSON error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// writeJSON encodes v as the response body with the given status. The body is
// marshalled first so an encoding failure can still produce a valid envelope.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"response encoding failed"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
