// xml.go — the OCS XML envelope writer and its marshalable shapes.
//
// Wire shape (classic ownCloud server, compact single-line XML):
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<ocs><meta><status>ok</status><statuscode>200</statuscode>
//	</meta><data>…</data></ocs>
//
// Status semantics (pinned by golden tests):
//   - <status> is "ok" iff <statuscode> is 200, else "failure".
//   - v1 responses always use HTTP 200 (the statuscode lives only in the
//     envelope — classic v1 behavior); v2 mirrors the statuscode in the
//     HTTP status (401/404/405 pass through).
package owncloud

import (
	"encoding/xml"
	"net/http"
	"strings"
)

// OCS statuscodes (classic server conventions; pinned by golden tests).
const (
	ocsStatusOK           = 200
	ocsStatusNotFound     = 404
	ocsStatusNotAllowed   = 405
	ocsStatusInternal     = 500
	ocsStatusUnauthorised = 997 // OCS convention for "no valid credentials"
)

// ocsContentType is the classic server's OCS response content type.
const ocsContentType = "text/xml; charset=UTF-8"

// xmlHeader is the XML declaration line (xml.Header minus its trailing
// newline; Encode closes the document without one).
const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// ocsData is the envelope's <data> block. Payloads are pre-marshaled and
// spliced as innerxml so each endpoint owns its payload shape.
type ocsData struct {
	XMLName xml.Name `xml:"data"`
	Inner   []byte   `xml:",innerxml"`
}

// ocsEnvelope is the <ocs> wrapper. Field order fixes the wire shape:
// meta (status, statuscode, message) then data.
type ocsEnvelope struct {
	XMLName    xml.Name `xml:"ocs"`
	Status     string   `xml:"meta>status"`
	StatusCode int      `xml:"meta>statuscode"`
	Message    string   `xml:"meta>message"`
	Data       *ocsData
}

// ocsVersionHeaderValue distinguishes the two served prefixes. VALUE
// UNVERIFIED: no real-client capture exists (leaf 01 adapted scope — see
// decision.md §1). The classic line advertises "1.7" on v1.php and "2.0"
// on v2.php; the manual real-client procedure (decision.md §6) should
// confirm a real client tolerates these before treating them as frozen.
func ocsVersionHeaderValue(version int) string {
	if version == 1 {
		return "1.7"
	}
	return "2.0"
}

// payload is the writeOCS data argument for multi-part payloads: each
// element is marshaled separately and concatenated inside <data> (the
// classic document is an unwrapped sequence: <data><version>…</version>
// <capabilities>…</capabilities></data>).
type payload []any

// writeOCS renders the envelope. httpStatus is the v2 HTTP status; v1
// always answers HTTP 200 (mapping rule above). data may be nil (empty
// <data></data> block — errors keep the element so clients parsing
// /ocs/.*/data never see a missing node).
func writeOCS(w http.ResponseWriter, ocsVersion, httpStatus, statusCode int, message string, data any) {
	var inner []byte
	switch d := data.(type) {
	case nil:
	case payload:
		for _, part := range d {
			raw, err := xml.Marshal(part)
			if err != nil {
				raw = nil // server bug; degrade to an empty block (see below)
			}
			inner = append(inner, raw...)
		}
	default:
		raw, err := xml.Marshal(d)
		if err != nil {
			// A payload that cannot marshal is a server bug; degrade to
			// an empty data block rather than a half-written envelope.
			raw = nil
		}
		inner = raw
	}
	block := &ocsData{Inner: inner}
	status := "failure"
	if statusCode == ocsStatusOK {
		status = "ok"
	}
	env := ocsEnvelope{Status: status, StatusCode: statusCode, Message: message, Data: block}

	w.Header().Set("Content-Type", ocsContentType)
	w.Header().Set("OCS-Version", ocsVersionHeaderValue(ocsVersion))
	code := httpStatus
	if ocsVersion == 1 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	var b strings.Builder
	b.WriteString(xmlHeader)
	enc := xml.NewEncoder(&b)
	if err := enc.Encode(env); err != nil {
		return // headers already sent; nothing safe to do
	}
	_, _ = w.Write([]byte(b.String()))
}
