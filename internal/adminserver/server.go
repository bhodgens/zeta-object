package adminserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/bhodgens/zeta-object/internal/adminserver/gateway"
)

// server.go — the console HTTP surface: the unauthenticated static/login
// surface, the session/CSRF gate in front of every /api/ path, and the
// transparent proxy of the gateway's management routes (Contract 2).
//
// The proxy adds nothing and removes nothing: it strips the console's /api
// prefix, forwards method, query, and body unchanged, and returns the
// gateway's status and body unchanged — including the gateway's own error
// envelope. A failure to reach the gateway is a 502 carrying the reason, never
// a fabricated success.

// errorEnvelope is the console's JSON error shape: an object with a top-level
// "error" key holding string "code" and "message" fields (Contract 2).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxProxyBodyBytes bounds a proxied request body read into memory. The
// gateway's own management body limit is smaller; this only stops the console
// from reading an unbounded stream before forwarding.
const maxProxyBodyBytes = 1 << 20 // 1 MiB

// Gateway is the console's typed client over the gateway's management routes
// (implemented by internal/adminserver/gateway.Client). Defining it here keeps
// the console's routing testable with a stub and decouples the HTTP surface
// from the transport.
type Gateway interface {
	Status(context.Context, url.Values) (json.RawMessage, error)
	GetConfig(context.Context, url.Values) (json.RawMessage, error)
	PutConfig(context.Context, json.RawMessage, url.Values) (json.RawMessage, error)
	SaveConfig(context.Context, url.Values) (json.RawMessage, error)
	ReloadAuth(context.Context, url.Values) (json.RawMessage, error)
	ListBuckets(context.Context, url.Values) (json.RawMessage, error)
	CreateBucket(context.Context, json.RawMessage, url.Values) (json.RawMessage, error)
	GetBucket(context.Context, string, url.Values) (json.RawMessage, error)
	DeleteBucket(context.Context, string, url.Values) (json.RawMessage, error)
	PutBucketSettings(context.Context, string, json.RawMessage, url.Values) (json.RawMessage, error)
	Purge(context.Context, json.RawMessage, url.Values) (json.RawMessage, error)
}

// apiRoute is one console /api/ route: a single allowed method and a path
// pattern whose segments may carry one "{name}" placeholder.
type apiRoute struct {
	method  string
	pattern []string
	handler http.HandlerFunc
}

// apiParamsKey is the context key under which apiHandler stores a matched
// route's path parameters for the handler.
type apiParamsKey struct{}

// Server is the console HTTP server.
type Server struct {
	cfg      *Config
	sessions *sessionStore
	static   *staticHandler
	gw       Gateway

	mu     sync.Mutex
	routes []apiRoute
}

// NewServer builds a Server from a validated config and its gateway client. It
// fails if cfg or gw is nil, or if the per-process session key cannot be
// generated. The gateway client is injected so the HTTP surface can be tested
// against a stub; production passes a *gateway.Client built from the console
// config's mTLS material.
func NewServer(cfg *Config, gw Gateway) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("adminserver: nil config")
	}
	if gw == nil {
		return nil, errors.New("adminserver: nil gateway client")
	}
	store, err := newSessionStore()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		sessions: store,
		static:   newStaticHandler(webFS),
		gw:       gw,
	}
	s.registerProxyRoutes()
	return s, nil
}

// registerProxyRoutes wires one handler per Contract 2 row.
func (s *Server) registerProxyRoutes() {
	routes := []struct {
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{http.MethodGet, "/api/status", s.handleAPIStatus},
		{http.MethodGet, "/api/config", s.handleAPIGetConfig},
		{http.MethodPut, "/api/config", s.handleAPIPutConfig},
		{http.MethodPost, "/api/config/save", s.handleAPISaveConfig},
		{http.MethodPost, "/api/auth/reload", s.handleAPIReloadAuth},
		{http.MethodGet, "/api/buckets", s.handleAPIListBuckets},
		{http.MethodPost, "/api/buckets", s.handleAPICreateBucket},
		{http.MethodGet, "/api/buckets/{name}", s.handleAPIGetBucket},
		{http.MethodDelete, "/api/buckets/{name}", s.handleAPIDeleteBucket},
		{http.MethodPut, "/api/buckets/{name}/settings", s.handleAPIPutBucketSettings},
		{http.MethodPost, "/api/purge", s.handleAPIPurge},
	}
	for _, rt := range routes {
		s.HandleAPI(rt.method, rt.path, rt.handler)
	}
}

// Handler returns the console's root HTTP handler.
//
// The unauthenticated surface is EXACTLY the embedded asset set (GET /,
// GET /assets/{file}) and the sign-in page / exchange (GET/POST /login). Every
// other path is either the /api/ gate or a static 404. The dispatch is explicit
// (no http.ServeMux) so an encoded or dotted asset path is never "cleaned" into
// a redirect and the 404/405 shapes come from one place.
func (s *Server) Handler() http.Handler {
	api := s.sessionGate(s.apiHandler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/":
			s.static.ServeHTTP(w, r)
		case p == "/login":
			s.handleLogin(w, r)
		case p == "/logout":
			s.handleLogout(w, r)
		case p == "/api" || strings.HasPrefix(p, "/api/"):
			api.ServeHTTP(w, r)
		default:
			s.static.ServeHTTP(w, r)
		}
	})
}

// HandleAPI registers an /api/ route behind the session gate. path is the full
// console path and may carry one "{name}" placeholder (for example
// "/api/buckets/{name}"); method is the single allowed method for that route.
func (s *Server) HandleAPI(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, apiRoute{
		method:  method,
		pattern: splitPath(path),
		handler: h,
	})
}

// apiHandler routes a gated /api/ request. The path is matched segment-wise
// against the registered patterns; a known path with a wrong method is a 405
// carrying an Allow header, an unknown path is a 404, both with the JSON
// envelope.
func (s *Server) apiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		segs := splitPath(r.URL.Path)

		s.mu.Lock()
		routes := append([]apiRoute(nil), s.routes...)
		s.mu.Unlock()

		var allowed []string
		for _, rt := range routes {
			params, ok := matchRoute(rt.pattern, segs)
			if !ok {
				continue
			}
			if rt.method == r.Method {
				if params != nil {
					r = r.WithContext(context.WithValue(r.Context(), apiParamsKey{}, params))
				}
				rt.handler(w, r)
				return
			}
			if !slices.Contains(allowed, rt.method) {
				allowed = append(allowed, rt.method)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this route")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
}

// sessionGate requires a valid session on every request and, for mutating
// requests, a matching CSRF header. Both checks run before any proxy handler
// (and therefore before any gateway call). Failures use the JSON error
// envelope: 401 when a session is absent, 403 when CSRF is missing or wrong.
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
// session. The token may arrive as a JSON body field "token" (the shell posts
// JSON) or as a form field named "token" (the form fallback). The token and the
// derived session secret are never logged.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid sign-in request")
		return
	}
	token, ok := extractLoginToken(raw)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid sign-in request")
		return
	}
	if !tokenEqual(token, s.cfg.OperatorToken) {
		writeError(w, http.StatusUnauthorized, "invalid_token", "operator token is not valid")
		return
	}
	csrf, err := s.sessions.mint(w, r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not start a session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrfToken": csrf})
}

// extractLoginToken reads the operator token from a JSON body field "token" or
// a form field named "token". The second return is false when the body is
// neither a well-formed JSON value nor a form carrying a token, so a malformed
// request is a 400 rather than a silent 401.
func extractLoginToken(raw []byte) (string, bool) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		return body.Token, true
	}
	if vals, err := url.ParseQuery(string(raw)); err == nil {
		if v, present := vals["token"]; present && len(v) > 0 {
			return v[0], true
		}
	}
	return "", false
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

// --- Proxy handlers (one per Contract 2 row) --------------------------------
//
// Each handler calls exactly the matching gateway method with the request's
// query (and body, for mutating routes) forwarded verbatim, then writes the
// gateway's response unchanged.

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.Status(r.Context(), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIGetConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.GetConfig(r.Context(), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIPutConfig(w http.ResponseWriter, r *http.Request) {
	body, ok := readProxyBody(w, r)
	if !ok {
		return
	}
	raw, err := s.gw.PutConfig(r.Context(), body, r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPISaveConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.SaveConfig(r.Context(), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIReloadAuth(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.ReloadAuth(r.Context(), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIListBuckets(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.ListBuckets(r.Context(), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPICreateBucket(w http.ResponseWriter, r *http.Request) {
	body, ok := readProxyBody(w, r)
	if !ok {
		return
	}
	raw, err := s.gw.CreateBucket(r.Context(), body, r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIGetBucket(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.GetBucket(r.Context(), apiParam(r, "name"), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIDeleteBucket(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gw.DeleteBucket(r.Context(), apiParam(r, "name"), r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIPutBucketSettings(w http.ResponseWriter, r *http.Request) {
	body, ok := readProxyBody(w, r)
	if !ok {
		return
	}
	raw, err := s.gw.PutBucketSettings(r.Context(), apiParam(r, "name"), body, r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

func (s *Server) handleAPIPurge(w http.ResponseWriter, r *http.Request) {
	body, ok := readProxyBody(w, r)
	if !ok {
		return
	}
	raw, err := s.gw.Purge(r.Context(), body, r.URL.Query())
	s.writeGatewayResult(w, raw, err)
}

// readProxyBody reads a bounded request body and returns it verbatim as JSON.
// An empty body returns nil (the gateway's methods emit no body then). A read
// failure is a 400 envelope and the second return is false.
func readProxyBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read request body")
		return nil, false
	}
	if len(raw) == 0 {
		return nil, true
	}
	return json.RawMessage(raw), true
}

// writeGatewayResult writes a gateway success (its status and body verbatim) or
// maps a gateway failure onto the console's error envelope.
func (s *Server) writeGatewayResult(w http.ResponseWriter, raw json.RawMessage, err error) {
	if err != nil {
		s.writeGatewayFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if len(raw) > 0 {
		// #nosec G705 -- raw is the gateway's own JSON response forwarded
		// verbatim (Contract 2); it is never HTML and the console does not
		// interpret it.
		_, _ = w.Write(raw)
	}
}

// writeGatewayFailure maps a gateway client error onto the console envelope. A
// non-2xx gateway response keeps its status and its own code/message; a
// transport failure (or any other exchange failure) is a 502 carrying the
// reason, so the console never pretends the gateway answered.
func (s *Server) writeGatewayFailure(w http.ResponseWriter, err error) {
	if he, ok := errors.AsType[*gateway.HTTPError](err); ok {
		code := he.Code
		if code == "" {
			code = "gateway_error"
		}
		writeError(w, he.Status, code, he.Message)
		return
	}
	if te, ok := errors.AsType[*gateway.TransportError](err); ok {
		writeError(w, http.StatusBadGateway, "gateway_unreachable", te.Reason)
		return
	}
	writeError(w, http.StatusBadGateway, "gateway_unreachable", err.Error())
}

// --- Routing helpers --------------------------------------------------------

// splitPath splits a URL path into its non-empty segments, dropping the leading
// slash: "/api/status" -> ["api","status"], "/" -> nil.
func splitPath(p string) []string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchRoute reports whether segs matches pattern and, when the pattern carries
// a "{name}" placeholder, returns the captured parameters. A placeholder never
// matches an empty segment.
func matchRoute(pattern, segs []string) (map[string]string, bool) {
	if len(pattern) != len(segs) {
		return nil, false
	}
	var params map[string]string
	for i, p := range pattern {
		if name, ok := placeholderName(p); ok {
			if segs[i] == "" {
				return nil, false
			}
			if params == nil {
				params = make(map[string]string, 1)
			}
			params[name] = segs[i]
			continue
		}
		if p != segs[i] {
			return nil, false
		}
	}
	return params, true
}

// placeholderName returns the name of a "{name}" pattern segment.
func placeholderName(seg string) (string, bool) {
	if len(seg) >= 3 && strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return seg[1 : len(seg)-1], true
	}
	return "", false
}

// apiParam returns a captured path parameter from the request context (empty
// when absent).
func apiParam(r *http.Request, name string) string {
	if params, ok := r.Context().Value(apiParamsKey{}).(map[string]string); ok {
		return params[name]
	}
	return ""
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
