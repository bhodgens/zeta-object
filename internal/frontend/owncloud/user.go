// user.go — the OCS /cloud/user endpoint (leaf 02 Task 4, adapted subset).
//
// The desktop client probes cloud/user during account setup for the
// display name it shows in the UI. zeta-object's identity model
// (auth.Identity) carries only an access key ID and bucket grants — there
// is no email or display-name field anywhere in the seam. HONEST ANSWER:
// the document reports the fields that exist (id = authenticated access
// key) and OMITS the fields that do not (display-name, email) — omitted
// elements, never invented values.
package owncloud

import (
	"encoding/xml"
	"net/http"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// ocUserPayload marshals to the bare <id> element (a document without a
// wrapper: <data><id>alice</id></data>). Only fields the auth seam can
// actually supply are present.
type ocUserPayload struct {
	XMLName xml.Name `xml:"id"`
	Value   string   `xml:",chardata"`
}

// ocUserPayloadFor builds the user document from an authenticated
// identity.
func ocUserPayloadFor(id auth.Identity) ocUserPayload {
	return ocUserPayload{Value: identityDisplayName(id)}
}

// handleUser serves GET /ocs/v{1,2}.php/cloud/user. Authentication has
// already run in routeOCS (every OCS request is authenticated); the
// identity arrives through the same call path.
func (f *Frontend) handleUser(w http.ResponseWriter, ocsVersion int, r *http.Request) {
	identity, _ := f.authnr.Authenticate(r)
	writeOCS(w, ocsVersion, http.StatusOK, ocsStatusOK, "OK", ocUserPayloadFor(identity))
}

// identityDisplayName derives the user document's id. The auth seam has
// no separate display name; the access key ID IS the identity (same value
// the Basic-auth username carries).
func identityDisplayName(id auth.Identity) string {
	return id.AccessKeyID
}
