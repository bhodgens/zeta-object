package s3

import (
	"encoding/xml"
	"log"
	"net/http"
	"strconv"
)

// xml.go — S3 XML error responses, response writer helper, ACL stub

// s3XMLNamespace is the S3 API XML namespace. It is pinned per-struct via the
// XMLName tag in types.go (e.g. `xml:"<ns> Error"`); Go's encoder only honors
// the tag, not runtime XMLName values. TestXMLNamespaceConstantPinned pins the
// constant to the tag value so the two cannot drift.
const s3XMLNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// errorToXML converts an error code and message to S3 XML error format
// (body only, with the S3 xmlns pinned via XMLName.Space; the XML prolog is
// added by writeXML/writeS3Error).
func errorToXML(code, message string) string {
	s3Err := S3Error{
		Code:    code,
		Message: message,
	}
	x, err := xml.MarshalIndent(s3Err, "", "  ")
	if err != nil {
		log.Printf("Error marshalling S3Error to XML: %v", err)
		return "<Error><Code>InternalError</Code><Message>Failed to generate error XML.</Message></Error>"
	}
	return string(x)
}

// writeXML writes v as an S3-conformant XML response: sets the Content-Type,
// prefixes the body with the XML prolog (xml.Header), and expects v to carry
// the S3 xmlns via its XMLName.Space (set by the caller, or by errorToXML for
// error documents).
func writeXML(w http.ResponseWriter, status int, v any) {
	x, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("Error marshalling %T to XML: %v", v, err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(x)
}

// writeXMLBytes writes pre-rendered XML bytes as an S3-conformant XML
// response — the same prolog + Content-Type treatment writeXML applies to
// encoded values, for callers whose body comes pre-rendered (e.g.
// objectmodel.TagsToXML; routing it through writeXML's encoder would
// escape the document). tagging tree leaf 03.
func writeXMLBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	//nolint:gosec // G705: callers pass pre-rendered XML (objectmodel.TagsToXML xml-escapes all content); this helper exists precisely for already-encoded documents.
	_, _ = w.Write(body)
}

// Placeholder ACL related requests.
func handleACL(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	log.Printf("ACL request for Bucket: '%s', Object: '%s' - Not Implemented", strconv.Quote(bucketName), strconv.Quote(objectName))
	writeS3Error(w, "NotImplemented", "ACLs are not implemented.", http.StatusNotImplemented)
}

// writeS3Error writes an S3-compliant XML error response (XML prolog +
// xmlns) with proper Content-Type header. The message may carry request-
// derived text, but errorToXML routes it through xml.MarshalIndent, which
// XML-escapes all special characters — the XSS taint is neutralized at the
// encoder (G705 false positive).
func writeS3Error(w http.ResponseWriter, code string, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write([]byte(errorToXML(code, message))) //nolint:gosec // G705: message is XML-escaped by MarshalIndent inside errorToXML.
}
