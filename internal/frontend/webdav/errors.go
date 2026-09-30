// errors.go — the centralized objectmodel.Error → HTTP status map and the
// RFC 4918 error rendering. davStatus is the ONLY place objectmodel codes
// map to statuses (webdav-2026-09 master Contract 4).
package webdav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// davStatus maps an objectmodel error code onto the WebDAV HTTP status
// (master Contract 4): NoSuchKey/NoSuchBucket ⇒ 404, PreconditionFailed ⇒
// 412, BucketAlreadyExists ⇒ 405 (MKCOL "exists"), InvalidArgument ⇒ 400,
// AccessDenied ⇒ 403, NotModified ⇒ 304, NotImplemented ⇒ 501. Unknown
// codes (and non-objectmodel errors) ⇒ 500 — never a silently guessed
// status.
func davStatus(err error) int {
	oe, ok := errors.AsType[*objectmodel.Error](err)
	if !ok {
		return http.StatusInternalServerError
	}
	switch oe.Code {
	case objectmodel.CodeNoSuchKey, objectmodel.CodeNoSuchBucket:
		return http.StatusNotFound
	case objectmodel.CodePreconditionFailed:
		return http.StatusPreconditionFailed
	case objectmodel.CodeBucketAlreadyExists:
		return http.StatusMethodNotAllowed
	case objectmodel.CodeInvalidArgument:
		return http.StatusBadRequest
	case objectmodel.CodeAccessDenied:
		return http.StatusForbidden
	case objectmodel.CodeNotModified:
		return http.StatusNotModified
	case objectmodel.CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// writeDavError renders an RFC 4918 <D:error> body with an optional
// precondition element (e.g. "propfind-finite-depth"), always
// application/xml; charset="utf-8".
func writeDavError(w http.ResponseWriter, status int, precondition string) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:error xmlns:D="DAV:">%s</D:error>`, preconditionBody(precondition))
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeDavErrorFrom maps err through davStatus and renders it.
func writeDavErrorFrom(w http.ResponseWriter, err error) int {
	status := davStatus(err)
	msg := "internal error"
	if oe, ok := errors.AsType[*objectmodel.Error](err); ok {
		msg = oe.Message
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<D:error xmlns:D="DAV:"><D:responsedescription>%s</D:responsedescription></D:error>`, xmlEscape(msg))
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return status
}

// preconditionBody renders a named precondition element (empty stays empty).
func preconditionBody(name string) string {
	if name == "" {
		return ""
	}
	return "<D:" + name + "/>"
}

// xmlEscape escapes text for XML character data (minimal set).
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
