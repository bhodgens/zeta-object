// lockhandler.go — leaf 03 (webdav-locking-2026-10): LOCK/UNLOCK (RFC
// 4918 §9.10/§9.11) over the lockStore seam (leaf 01) and the write-path
// enforcement hook built on CheckWriteLock (leaf 02).
//
// v1 scope (master brief + README):
//   - exclusive write locks only; shared lockscope is a documented 400;
//   - Depth 0 only; an explicit Depth: infinity is a documented 400;
//   - Timeout: Second-N honored, capped at 3600s (default 600s);
//   - LOCK response: 200 + Lock-Token header + prop/lockdiscovery body;
//   - conflicting LOCK → 423; writes without the token → 423;
//   - UNLOCK correct token → 204, wrong/missing token → 409 (the brief
//     pins 409 Conflict for a token mismatch; RFC 4918 §9.11.1 suggests
//     403 for a client with no authority over the lock — the brief's
//     409 is chosen and documented here);
//   - v1 locks FILES only: LOCK on a collection URL or the root is 405
//     (davfs2 directory-lock open question resolved as "document, don't
//     emulate"; dir-marker locks are a possible follow-up leaf);
//   - LOCK on an unmapped URL locks the NAME without creating an empty
//     resource (RFC 4918 §9.10.4's empty-resource creation is not
//     emulated — the resource materializes on the token-carrying PUT).
//
// Locks are server-side advisory coordination state under the bucket's
// existing .metadata/ area (charter-checked in lockstore.go).
package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// maxLockTimeout is the hard lease cap in seconds: a client asking
	// for more (or "Infinite") gets the cap.
	maxLockTimeout = int64(3600)
	// defLockTimeout applies when the Timeout header is absent, is
	// "Infinite", or carries no parseable Second-N value.
	defLockTimeout = int64(600)
	// maxLockBodySize bounds the LOCK request body read.
	maxLockBodySize = 64 << 10
)

// lockinfoXML is the RFC 4918 §9.10.6 request body. Scope/type members
// are pointers so absence (lenient: default exclusive write) is
// distinguishable from an explicit unsupported request.
type lockinfoXML struct {
	XMLName   xml.Name `xml:"lockinfo"`
	LockScope struct {
		Exclusive *struct{} `xml:"exclusive"`
		Shared    *struct{} `xml:"shared"`
	} `xml:"lockscope"`
	LockType struct {
		Write *struct{} `xml:"write"`
	} `xml:"locktype"`
	Owner struct {
		Href string `xml:"href"`
	} `xml:"owner"`
}

// parseLockTimeoutHeader interprets the Timeout header ("Infinite",
// "Second-N", or a comma list) into the granted lease: the largest
// parseable Second-N value the client asked for, capped at
// maxLockTimeout; anything unparseable (including "Infinite" or an
// absent header) gets defLockTimeout.
func parseLockTimeoutHeader(h string) int64 {
	requested := int64(-1)
	for part := range strings.SplitSeq(h, ",") {
		part = strings.TrimSpace(part)
		rest, ok := strings.CutPrefix(strings.ToLower(part), "second-")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(rest, 10, 64); err == nil && n > requested {
			requested = n
		}
	}
	if requested < 0 {
		return defLockTimeout
	}
	if requested > maxLockTimeout {
		return maxLockTimeout
	}
	return requested
}

// lockDepthOK reports whether the LOCK Depth header is acceptable: v1
// serves Depth 0 only (an absent Depth is treated as 0 — lenient,
// documented deviation from the RFC's infinity default).
func lockDepthOK(h string) bool {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "", "0":
		return true
	default:
		return false
	}
}

// lockKey maps a resource onto the lockStore key: slash-led bucket+key
// (unique per bucket in both modes; the store lives under that bucket's
// own .metadata directory).
func (f *Frontend) lockKey(res resource) string {
	return "/" + res.bucket + "/" + res.key
}

// lockStoreFor returns the per-bucket lockStore, constructing it lazily
// under <lockRoot(bucket)>/.metadata/. nil when locking is not wired
// (WithLockStoreRoot absent) — LOCK/UNLOCK then answer 405 and write
// enforcement is a no-op, matching the pre-locking frontend exactly.
func (f *Frontend) lockStoreFor(bucket string) lockStore {
	if f.lockRoot == nil {
		return nil
	}
	f.locksMu.Lock()
	defer f.locksMu.Unlock()
	if s, ok := f.locks[bucket]; ok {
		return s
	}
	s := newFileLockStore(filepath.Join(f.lockRoot(bucket), ".metadata"))
	if f.locks == nil {
		f.locks = make(map[string]lockStore)
	}
	f.locks[bucket] = s
	return s
}

// enforceWriteLock gates the write paths: false means the handler must
// stop (the response is already written — 423 for a live lock the request
// does not hold, 500 for a store failure, which must never silently pass).
// Lock-management methods never call this: LOCK manages locks, UNLOCK
// removes one, and both are exempt from enforcement (master brief).
func (f *Frontend) enforceWriteLock(w http.ResponseWriter, r *http.Request, res resource) bool {
	if f.lockRoot == nil || res.key == "" {
		return true
	}
	err := CheckWriteLock(f.lockStoreFor(res.bucket), f.lockKey(res), r.Header.Get("If"))
	if err == nil {
		return true
	}
	if errors.Is(err, ErrLocked) {
		writeDavError(w, http.StatusLocked, "")
		return false
	}
	writeDavError(w, http.StatusInternalServerError, "")
	return false
}

// handleLOCK implements LOCK: create (body) or refresh (empty body + If
// token). Collection/root targets are 405 (v1 files only, documented).
func (f *Frontend) handleLOCK(w http.ResponseWriter, r *http.Request, res resource) {
	store := f.lockStoreFor(res.bucket)
	if store == nil || res.isRoot || res.isCollection || res.key == "" {
		w.Header().Set("Allow", allowHeader)
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	if !lockDepthOK(r.Header.Get("Depth")) {
		// Documented: Depth infinity (and any non-0 depth) unsupported.
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	timeout := parseLockTimeoutHeader(r.Header.Get("Timeout"))
	key := f.lockKey(res)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBodySize+1))
	if err != nil {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	if len(body) > maxLockBodySize {
		writeDavError(w, http.StatusRequestEntityTooLarge, "")
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		f.handleLOCKRefresh(w, r, store, key, timeout)
		return
	}
	var req lockinfoXML
	if err := xml.Unmarshal(body, &req); err != nil {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	if req.LockScope.Shared != nil {
		// Documented: shared locks unsupported in v1.
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	if req.LockType.Write == nil {
		// Only <write/> lock types exist in RFC 4918, but an explicit
		// non-write element is malformed for this server: 400.
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	info, err := store.Acquire(key, LockInfo{
		Owner:   req.Owner.Href,
		Depth:   "0",
		Timeout: timeout,
	})
	if errors.Is(err, ErrLocked) {
		writeDavError(w, http.StatusLocked, "")
		return
	}
	if err != nil {
		writeDavError(w, http.StatusInternalServerError, "")
		return
	}
	writeLockResponse(w, http.StatusOK, info, f.davPath(res))
}

// handleLOCKRefresh refreshes an existing lock (empty LOCK body + If
// header token, RFC 4918 §9.10.6). A missing/expired or non-matching
// token is 412 (the refresh precondition failed).
func (f *Frontend) handleLOCKRefresh(w http.ResponseWriter, r *http.Request, store lockStore, key string, timeout int64) {
	tokens := ParseIfHeader(r.Header.Get("If"))
	if len(tokens) == 0 {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	info, err := store.Refresh(key, tokens[0], timeout)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLockTokenMismatch) {
		writeDavError(w, http.StatusPreconditionFailed, "")
		return
	}
	if err != nil {
		writeDavError(w, http.StatusInternalServerError, "")
		return
	}
	writeLockResponse(w, http.StatusOK, info, f.davPath(f.resourceOfKey(key)))
}

// resourceOfKey reconstructs the client-visible path for a lock key
// ("/bucket/key") for the lockroot href in refresh responses.
func (f *Frontend) resourceOfKey(key string) resource {
	trimmed := strings.TrimPrefix(key, "/")
	bucket, rest, _ := strings.Cut(trimmed, "/")
	return resource{bucket: bucket, key: rest}
}

// handleUNLOCK implements UNLOCK (RFC 4918 §9.11): the Lock-Token header
// identifies the lock. Correct token → 204; wrong, missing, or a lock
// that is gone → 409 Conflict (the brief's pin, documented in the file
// header). Collection/root targets are 405 (v1 files only).
func (f *Frontend) handleUNLOCK(w http.ResponseWriter, r *http.Request, res resource) {
	store := f.lockStoreFor(res.bucket)
	if store == nil || res.isRoot || res.isCollection || res.key == "" {
		w.Header().Set("Allow", allowHeader)
		writeDavError(w, http.StatusMethodNotAllowed, "")
		return
	}
	token := strings.Trim(strings.TrimSpace(r.Header.Get("Lock-Token")), "<>")
	if token == "" {
		writeDavError(w, http.StatusBadRequest, "")
		return
	}
	err := store.Release(f.lockKey(res), token)
	switch {
	case err == nil:
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrLockTokenMismatch), errors.Is(err, ErrNotFound):
		writeDavError(w, http.StatusConflict, "")
	default:
		writeDavError(w, http.StatusInternalServerError, "")
	}
}

// writeLockResponse renders the RFC 4918 §9.10.6 success response: the
// Lock-Token header and the prop/lockdiscovery XML body.
func writeLockResponse(w http.ResponseWriter, status int, info LockInfo, root string) {
	body := lockdiscoveryXML(info, root)
	w.Header().Set("Lock-Token", "<"+info.Token+">")
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// lockdiscoveryXML renders the <D:prop><D:lockdiscovery> body describing
// the granted lock.
func lockdiscoveryXML(info LockInfo, root string) string {
	owner := ""
	if info.Owner != "" {
		owner = "<D:href>" + xmlEscape(info.Owner) + "</D:href>"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>
<D:locktype><D:write/></D:locktype>
<D:lockscope><D:exclusive/></D:lockscope>
<D:depth>%s</D:depth>
<D:owner>%s</D:owner>
<D:timeout>Second-%d</D:timeout>
<D:locktoken><D:href>%s</D:href></D:locktoken>
<D:lockroot><D:href>%s</D:href></D:lockroot>
</D:activelock></D:lockdiscovery></D:prop>`,
		xmlEscape(info.Depth), owner, info.Timeout, xmlEscape(info.Token), xmlEscape(root))
}
