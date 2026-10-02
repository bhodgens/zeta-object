// lockif.go — leaf 02 (webdav-locking-2026-10): Contract 2, RFC 4918
// §10.4 If-header parsing and write-lock enforcement. The parser extracts
// every opaquelocktoken: URI submitted in the header (single token, token
// list, or tagged list form); malformed headers yield an empty list so
// the caller sees "no tokens submitted". CheckWriteLock maps that onto
// the lockStore seam: free resources pass, locked resources require a
// submitted token that matches the holder.
package webdav

import (
	"errors"
	"slices"
	"strings"
)

// ParseIfHeader parses an RFC 4918 §10.4 If header and returns every
// opaquelocktoken: URI it submits, in order of appearance. The tagged
// list form ("If: <da:href> (<opaquelocktoken:...>)") and the Not
// operator are tolerated: tagged resource markers and non-token entities
// (ETags) are skipped; only opaquelocktoken URIs are extracted. A
// malformed or empty header returns an empty list (nil), which callers
// treat as "no tokens submitted".
func ParseIfHeader(h string) []string {
	// Structural pre-check: a header with unbalanced parentheses is
	// malformed and yields an empty list (Contract 2).
	depth := 0
	for _, r := range h {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil
			}
		}
	}
	if depth != 0 {
		return nil
	}
	var tokens []string
	rest := h
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		closeIdx := strings.IndexByte(rest[open+1:], '>')
		if closeIdx < 0 {
			// Unterminated entity: header is malformed; keep tokens
			// already extracted, stop scanning.
			return tokens
		}
		entity := rest[open+1 : open+1+closeIdx]
		rest = rest[open+1+closeIdx+1:]
		if strings.HasPrefix(entity, "opaquelocktoken:") {
			tokens = append(tokens, entity)
		}
	}
	return tokens
}

// CheckWriteLock reports whether the resource at key may be written
// (Contract 2): nil when unlocked, or locked AND one of the tokens in
// ifHeader matches the lock holder's token; ErrLocked otherwise (mapped
// to 423 by the handler layer). A malformed or empty ifHeader counts as
// "no tokens submitted".
func CheckWriteLock(store lockStore, key string, ifHeader string) error {
	info, err := store.Get(key)
	if err != nil {
		// ErrNotFound (including wrapped variants from the file store)
		// covers "never locked" and "expired" — the store's lazy expiry
		// already removed expired locks, so the resource is writable.
		// Any other store failure is reported as-is rather than guessed.
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	}
	if slices.Contains(ParseIfHeader(ifHeader), info.Token) {
		return nil
	}
	return newLockErr(ErrLocked, "%s is locked", key)
}
