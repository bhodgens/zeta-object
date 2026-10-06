// dispatch.go — the request pipeline: authenticate (401 challenge) →
// authorize (grant gate) → resolve resource → method dispatch. Auth runs
// BEFORE method dispatch so an unauthenticated request 401s on every method
// (including LOCK: challenge first, 405 only after auth — leaf 04 rule).
package webdav

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// allowHeader is the Allow value advertised on OPTIONS and on 405s.
const allowHeader = "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, MKCOL, COPY, MOVE, LOCK, UNLOCK"

// realm is the frozen Basic-auth realm string (leaf 04 note: changing it
// breaks saved client credentials).
const realm = `Basic realm="zeta-object"`

// identityKey is the unexported context key threading the authenticated
// Identity from the auth stage to per-method handlers (documented choice:
// context over extra parameters, matching the handler-method signatures).
type identityKey struct{}

// serveHTTP is the pipeline entry.
func (f *Frontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := f.authenticate(w, r)
	if !ok {
		return
	}
	ctx := context.WithValue(r.Context(), identityKey{}, identity)

	res, ok := f.parseResource(r.URL.Path)
	if !ok {
		// W1: a dot-segment bucket/key would escape dataDir through the
		// fs backend's unchecked join — reject before any backend call.
		writeDavError(w, http.StatusForbidden, "")
		return
	}
	if !f.authorize(w, r.WithContext(ctx), identity, res) {
		return
	}

	// quic-h3-2026-10 leaf 07: the JSON batch surface (POST ?batch)
	// resolves BEFORE the method switch — a POST sub-resource with the
	// bucket scope resolved (same pre-dispatch position as the GET-query
	// zfsSurfaceDispatch). Auth and grants ran above; a GET ?batch is
	// the mount's 405. Unmatched (no ?batch param) falls through
	// unchanged.
	if f.batchDispatch(w, r.WithContext(ctx), res) {
		return
	}

	switch r.Method {
	case "OPTIONS":
		f.handleOPTIONS(w, r)
	case "GET":
		// Leaf 06: the ZFS enrichment queries (?events on any GET,
		// ?versions on a file GET) resolve BEFORE resource-kind
		// resolution — a query on a collection must not fall into the
		// collection-GET path. Unknown query params are ignored here
		// (zfsSurfaceDispatch returns false) and take the plain path
		// below, byte-unchanged. Auth and grants ran above.
		if f.zfsSurfaceDispatch(w, r.WithContext(ctx), res) {
			return
		}
		f.handleGET(w, r.WithContext(ctx), res, false)
	case "HEAD":
		if f.zfsSurfaceDispatch(w, r.WithContext(ctx), res) {
			return
		}
		f.handleGET(w, r.WithContext(ctx), res, true)
	case "PROPFIND":
		f.handlePROPFIND(w, r.WithContext(ctx), res)
	case "PUT":
		if !f.enforceWriteLock(w, r, res) {
			return
		}
		f.handlePUT(w, r.WithContext(ctx), res)
	case "DELETE":
		if !f.enforceWriteLock(w, r, res) {
			return
		}
		f.handleDELETE(w, r.WithContext(ctx), res)
	case "MKCOL":
		f.handleMKCOL(w, r.WithContext(ctx), res)
	case "COPY":
		f.handleCopyMoveDispatch(w, r.WithContext(ctx), res, false)
	case "MOVE":
		f.handleCopyMoveDispatch(w, r.WithContext(ctx), res, true)
	case "LOCK":
		f.handleLOCK(w, r.WithContext(ctx), res)
	case "UNLOCK":
		f.handleUNLOCK(w, r.WithContext(ctx), res)
	default:
		// PROPPATCH, VERSION-CONTROL, gibberish: a rejection, not
		// emulation (master Contract 4). Allow comes only after
		// successful auth.
		w.Header().Set("Allow", allowHeader)
		writeDavError(w, http.StatusMethodNotAllowed, "")
	}
}

// handleCopyMoveDispatch gates COPY/MOVE on lock keys before any
// delegation: the destination (both verbs write it) always; the source
// only for MOVE, whose delete half is a write on the source (RFC 4918
// §7.1). COPY reads the source and is never lock-gated on it (the brief:
// "COPY-dest"). The Destination header is re-parsed here only for the
// lock key; the authoritative Destination validation stays in
// handleCopyMove's own resolver. LOCK/UNLOCK never reach enforcement —
// lock management is exempt (master brief).
func (f *Frontend) handleCopyMoveDispatch(w http.ResponseWriter, r *http.Request, src resource, isMove bool) {
	if u, err := url.Parse(r.Header.Get("Destination")); err == nil && u.Path != "" {
		if dst, ok := f.parseResource(u.Path); ok && dst.key != "" {
			if !f.enforceWriteLock(w, r, dst) {
				return
			}
		}
	}
	if isMove && !f.enforceWriteLock(w, r, src) {
		return
	}
	f.handleCopyMove(w, r, src, isMove)
}

// authenticate runs the injected authenticator. Missing or invalid
// credentials ⇒ 401 + WWW-Authenticate; authenticator-internal failures ⇒
// 500 (credential failures are typed sentinels in internal/auth — anything
// else is a wiring bug, not a client error).
func (f *Frontend) authenticate(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, err := f.authnr.Authenticate(r)
	if err == nil {
		return id, true
	}
	if isCredentialRejection(err) {
		w.Header().Set("WWW-Authenticate", realm)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusUnauthorized)
		return auth.Identity{}, false
	}
	// Authenticator-internal failure: never log credentials; a 500 with
	// the generic error body is the only safe response.
	writeDavError(w, http.StatusInternalServerError, "")
	return auth.Identity{}, false
}

// isCredentialRejection classifies an authenticator error as a client-side
// credential rejection (⇒ 401 challenge) or as an internal fault (⇒ 500).
//
// The predicate is an EXPLICIT allow-list of internal/auth's typed rejection
// sentinels — never "any error". Both shipped adapters contribute: the four
// Basic sentinels and the three certificate sentinels, which the mTLS
// CertAuthenticator returns (ErrCertMissing with no peer certificate,
// ErrCertUnknownCN for an empty or unregistered CN, ErrCertRegistryNil when
// the adapter cannot consult a registry — all three fail closed). Listing
// only the Basic sentinels is what made every certificate rejection answer
// 500: a denied request read as a server fault, so zeta-cache and every
// retry/error-budget path retried and alerted on a bad certificate.
//
// errors.Is (not ==) so an adapter that annotates a rejection still matches.
// A new sentinel added in internal/auth must be added here too — that is the
// single place that decides 401-vs-500, and omitting a sentinel silently
// re-creates this bug.
func isCredentialRejection(err error) bool {
	return errors.Is(err, auth.ErrBasicMissing) ||
		errors.Is(err, auth.ErrBasicMalformed) ||
		errors.Is(err, auth.ErrBadCredentials) ||
		errors.Is(err, auth.ErrBasicUnsupported) ||
		errors.Is(err, auth.ErrCertMissing) ||
		errors.Is(err, auth.ErrCertUnknownCN) ||
		errors.Is(err, auth.ErrCertRegistryNil)
}

// authorize enforces the identity's grants against the effective bucket
// (master Contract 6): mode A first path segment, mode B the configured
// bucket. Grant check precedes existence — a 404 must never leak which
// paths exist to an identity without the grant. COPY/MOVE check source
// read and destination write independently (in handleCopyMove's own
// destination resolution, so here only the source/resource bucket is
// gated; the Destination gate happens there).
func (f *Frontend) authorize(w http.ResponseWriter, r *http.Request, id auth.Identity, res resource) bool {
	// OPTIONS is the capability probe: authenticated, no grant required
	// (pinned by the leaf-04 matrix).
	if r.Method == "OPTIONS" {
		return true
	}
	write := requestGrantsWrite(r.Method)
	if res.isRoot {
		return f.authorizeRoot(w, r, id, write)
	}
	if err := frontend.AuthorizeRequest(id, res.bucket, write); err != nil {
		writeDavError(w, http.StatusForbidden, "")
		return false
	}
	return true
}

// authorizeRoot applies the root-collection rule: mode B requires Read (or
// Write for a write — but the root rejects all writes later) on the
// configured bucket; mode A requires a grant on at least one bucket —
// listing buckets reveals names, so "no grants at all" is a 403 (leaf 04).
// OPTIONS needs no grant (pinned by leaf 04's matrix).
func (f *Frontend) authorizeRoot(w http.ResponseWriter, r *http.Request, id auth.Identity, write bool) bool {
	if r.Method == "OPTIONS" {
		return true
	}
	// The root's visibility check gates only the methods that reveal
	// bucket names (PROPFIND/GET/HEAD list). Rejections for root writes
	// (MKCOL/DELETE/PUT...) are the handlers' 405/403 per Contract 4 —
	// those reveal nothing.
	switch r.Method {
	case "PROPFIND", "GET", "HEAD":
	default:
		return true
	}
	if f.bucket != "" {
		if err := frontend.AuthorizeRequest(id, f.bucket, false); err != nil {
			writeDavError(w, http.StatusForbidden, "")
			return false
		}
		return true
	}
	// Mode A: listing buckets reveals names, so the root requires Read on
	// at least one bucket the server actually serves (matched against
	// Buckets(); a "*" wildcard grant always matches).
	buckets, err := f.be.Buckets(r.Context())
	if err != nil {
		writeDavErrorFrom(w, err)
		return false
	}
	for _, b := range buckets {
		if id.CanRead(b.Name) {
			return true
		}
	}
	writeDavError(w, http.StatusForbidden, "")
	return false
}

// requestGrantsWrite classifies methods as write-grant vs read-grant
// operations. COPY/MOVE require write on the destination AND read on the
// source; the destination check happens in handleCopyMove against the
// parsed Destination, so classification here only decides the source gate
// (read) — the write gate is enforced there before any Backend call.
func requestGrantsWrite(method string) bool {
	switch method {
	case "PUT", "DELETE", "MKCOL":
		return true
	case "MOVE":
		// MOVE's destructive half (source delete) needs Write on the
		// source bucket; COPY's source needs only Read.
		return true
	default:
		return false
	}
}

// handleOPTIONS advertises the DAV class-1 subset (leaf 02 Task 4).
func (f *Frontend) handleOPTIONS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", allowHeader)
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// identityOf pulls the authenticated identity from the request context.
func identityOf(r *http.Request) auth.Identity {
	id, _ := r.Context().Value(identityKey{}).(auth.Identity)
	return id
}
