// publickey.go — the SFTP public-key adapter hook (pluggable-authentication
// tree leaf 04). INTERFACE-ONLY: no SSH server, no wire protocol, no
// golang.org/x/crypto/ssh dependency (stdlib-only repo rule).
//
// Canonicalization is deliberately format-level: an authorized_keys line is
// split into fields, the optional options prefix is stripped, the keytype is
// lowercased, and the base64 blob is compared DECODED so whitespace and
// padding variants collapse to one canonical form. Fingerprints are SHA-256
// over the decoded blob bytes (OpenSSH-style "SHA256:<b64>") — never key
// material — so auth successes/failures can be logged safely.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrKeyMalformed marks input that is not a parseable authorized_keys-style
// key line. ErrKeyUnknown marks a valid key no identity registers.
var (
	ErrKeyMalformed = errors.New("auth: malformed ssh public key")
	ErrKeyUnknown   = errors.New("auth: ssh public key not registered")
)

// PublicKeyAuthenticator is the seam the future SFTP frontend consumes:
// normalize an authorized_keys-style line (or bare keytype+blob string) and
// resolve it to an identity. Implemented by MultiRegistry.
type PublicKeyAuthenticator interface {
	AuthenticatePublicKey(presentedKey string) (Identity, error)
}

// compile-time: MultiRegistry implements the hook.
var _ PublicKeyAuthenticator = (*MultiRegistry)(nil)

// AuthenticatePublicKey canonicalizes the presented key and resolves it
// through the registry. Malformed input → ErrKeyMalformed; valid but
// unregistered → ErrKeyUnknown; both are typed errors the SFTP frontend can
// render protocol-appropriately (never a silent miss).
func (r *MultiRegistry) AuthenticatePublicKey(presentedKey string) (Identity, error) {
	canonical, _, err := CanonicalizePublicKey(presentedKey)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrKeyMalformed, err)
	}
	st, ok := r.publicKeyOwner[canonical]
	if !ok {
		return Identity{}, ErrKeyUnknown
	}
	return st.identity, nil
}

// CanonicalizePublicKey normalizes an authorized_keys-format line to the
// canonical form stored/compared by the registry:
//
//	"<lowercased-keytype> <base64-blob>"
//
// Comments are stripped, the leading options field(s) of an authorized_keys
// line are stripped, and whitespace is collapsed. Quoted option values
// (command="...", environment="...") are parsed as ONE field (A3: a plain
// Fields split would find a keytype-looking token INSIDE a quoted option
// and bind the wrong blob). The returned fingerprint is
// "SHA256:<base64std-nopad>" of the decoded blob bytes (OpenSSH style) —
// log-safe, never the key material itself.
func CanonicalizePublicKey(presentedKey string) (canonical string, fingerprint string, err error) {
	line := strings.TrimSpace(presentedKey)
	if line == "" {
		return "", "", fmt.Errorf("empty key line")
	}
	fields := splitAuthorizedFields(line)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("want \"<keytype> <base64-blob> [comment]\", got %d field(s)", len(fields))
	}

	// An authorized_keys line may start with option fields (comma- or
	// space-separated assignments/flags, e.g. environment="X",
	// no-port-forwarding), in which case the keytype appears after them.
	// Options never look like a key algorithm name; the FIRST field naming
	// a known keytype starts the "<keytype> <blob>" pair. Fields after the
	// blob are the free-text comment (ignored).
	keytypeIdx, blobIdx := -1, -1
	for i, f := range fields {
		if keytypeIdx < 0 && !validKeytype(strings.ToLower(f)) {
			continue // option field
		}
		keytypeIdx = i
		if i+1 >= len(fields) {
			return "", "", fmt.Errorf("keytype %q has no base64 blob", f)
		}
		blobIdx = i + 1
		break
	}
	if keytypeIdx < 0 {
		return "", "", fmt.Errorf("no keytype field found")
	}

	keytype := strings.ToLower(fields[keytypeIdx])
	if !validKeytype(keytype) {
		return "", "", fmt.Errorf("unrecognized keytype %q", keytype)
	}
	blob, err := decodeKeyBlob(fields[blobIdx])
	if err != nil {
		return "", "", fmt.Errorf("key blob: %w", err)
	}
	sum := sha256.Sum256(blob)
	fp := "SHA256:" + base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(sum[:])
	// Canonical form re-encodes the DECODED blob with base64.StdEncoding, so
	// padding and alphabet variants of the same key collapse to one string.
	return keytype + " " + base64.StdEncoding.EncodeToString(blob), fp, nil
}

// splitAuthorizedFields splits an authorized_keys line into fields,
// respecting double-quoted option values: command="echo ssh-ed25519 evil"
// stays ONE field (a plain whitespace split would treat tokens inside the
// quotes as keytype+blob candidates and mis-bind the key). Quotes inside a
// quoted value ("" per sshd) toggle back out; an unterminated quote
// consumes to end-of-line, which then fails keytype validation loudly.
func splitAuthorizedFields(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	inField := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			inField = true
			cur.WriteByte(c)
		case c == '\\' && i+1 < len(line) && inQuote:
			// Backslash escape inside quotes (sshd rule): keep verbatim.
			cur.WriteByte(c)
			i++
			cur.WriteByte(line[i])
		case (c == ' ' || c == '	') && !inQuote:
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			inField = true
			cur.WriteByte(c)
		}
	}
	if inField {
		fields = append(fields, cur.String())
	}
	return fields
}

// validKeytype accepts the SSH public key algorithm names a real deployment
// would paste from ~/.ssh/*.pub (case-insensitive at the call site).
func validKeytype(keytype string) bool {
	switch keytype {
	case "ssh-ed25519", "ssh-rsa", "ssh-dss", "ecdsa-sha2-nistp256",
		"ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	}
	return false
}

// decodeKeyBlob base64-decodes the blob (standard or URL alphabet, padded or
// raw) so equivalent encodings collapse to the same bytes.
func decodeKeyBlob(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, fmt.Errorf("empty blob")
	}
	trimmed := strings.TrimRight(b64, "=")
	enc := base64.RawStdEncoding
	if strings.ContainsAny(trimmed, "-_") {
		enc = base64.RawURLEncoding
	}
	blob, err := enc.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("not valid base64")
	}
	if len(blob) == 0 {
		return nil, fmt.Errorf("blob decodes to zero bytes")
	}
	return blob, nil
}

// LogSafeKey renders one audit-log field set for a public-key auth event:
// "accessKeyID=<akid> fingerprint=<fp>". Key material and comments never
// appear — the fingerprint is already a one-way digest.
func LogSafeKey(accessKeyID, fingerprint string) string {
	return "accessKeyID=" + accessKeyID + " fingerprint=" + fingerprint
}
