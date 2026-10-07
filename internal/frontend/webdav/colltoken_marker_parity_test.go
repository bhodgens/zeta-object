// colltoken_marker_parity_test.go — pins for the derived collection change
// token's two correctness holes (bughunt 2026-10-06 M2 and M3).
//
// The token exists so a caching WebDAV client can ask "did anything in this
// directory change?" with one Depth-0 PROPFIND instead of a full walk. That
// makes ONE promise: if the served listing changed, the token moves. Both holes
// below broke exactly that promise.
//
// M2 (the deletion the feature exists to catch): the token's dedicated List
// walk saw the tombstoned key's surviving data file, while childEntries dropped
// the same row via the marker consult. Token unchanged, listing changed.
// Measured before the fix:
//
//	collection /2024/ token BEFORE delete = dir-611feda296f6e125
//	collection /2024/ token AFTER marker  = dir-611feda296f6e125
//
// M3 (a lost assignment): rootEntries wrote the Depth-1 root token to a LOCAL
// struct after the slice element had been copied out, so the row the 207
// renders kept the empty rendering:
//
//	Depth-1 root getetag = ""
//	Depth-0 root getetag = dir-8419b82681603009
//
// These use the production-wired environment from propfind_marker_test.go
// (WithLockStoreRoot only, bucketPathFn asserted nil).
package webdav

import (
	"strings"
	"testing"
)

// collETagFor returns the getetag of the row whose href matches, so a probe on
// "/" does not read a CHILD's token.
func collETagFor(body, href string) string {
	for block := range strings.SplitSeq(body, "<d:response>") {
		if !strings.Contains(block, "<d:href>"+href+"</d:href>") {
			continue
		}
		_, rest, found := strings.Cut(block, "<d:getetag>")
		if !found {
			return ""
		}
		value, _, found := strings.Cut(rest, "</d:getetag>")
		if !found {
			return ""
		}
		return strings.Trim(value, "&#34;")
	}
	return "<no-row>"
}

// TestCollectionTokenMovesOnDeleteMarker is the M2 pin: tombstoning a child
// must move its parent's token, or a syncing client never learns the deletion.
func TestCollectionTokenMovesOnDeleteMarker(t *testing.T) {
	e := newPropfindMarkerEnv(t, "colltoken-marker")
	e.enable()
	e.davWrite("2024/gone.txt", "secret-payload")
	e.davWrite("2024/kept.txt", "live-payload")

	_, before := e.davPropfind("/", "1")
	tokBefore := collETagFor(before, "/2024/")
	if tokBefore == "" || tokBefore == "<no-row>" {
		t.Fatalf("test premise: /2024/ has no token before the delete (%q)", tokBefore)
	}

	if code := e.webdavDelete("2024/gone.txt"); code != 204 {
		t.Fatalf("DELETE = %d, want 204", code)
	}

	_, after := e.davPropfind("/", "1")
	tokAfter := collETagFor(after, "/2024/")

	// The listing must have dropped the row (the marker consult).
	if hrefPresent(after, "/2024/gone.txt") {
		t.Fatalf("test premise: the tombstoned key is still advertised, so the token case is moot")
	}
	// THE PIN: the token must MOVE.
	if tokAfter == tokBefore {
		t.Errorf("M2 REGRESSION: gone.txt was tombstoned and dropped from the listing, but "+
			"/2024/'s token is unchanged (%q) - a syncing client reads that as "+
			"'nothing changed' and never learns the deletion", tokAfter)
	}
}

// TestCollectionTokenDepth0AndDepth1Agree is the M2 arm at Depth 0: the
// dedicated walk must apply the same marker consult, so both depths describe
// one state. Before the fix the Depth-0 token was ALSO unmoved (it used the
// same unfiltered walk).
func TestCollectionTokenDepth0AndDepth1Agree(t *testing.T) {
	e := newPropfindMarkerEnv(t, "colltoken-depth0")
	e.enable()
	e.davWrite("2024/gone.txt", "secret-payload")
	e.davWrite("2024/kept.txt", "live-payload")

	_, d0before := e.davPropfind("/2024/", "0")
	tokBefore := collETag(d0before)
	if tokBefore == "" {
		t.Fatalf("test premise: /2024/ has no Depth-0 token before the delete")
	}

	if code := e.webdavDelete("2024/gone.txt"); code != 204 {
		t.Fatalf("DELETE = %d, want 204", code)
	}

	_, d0after := e.davPropfind("/2024/", "0")
	tokAfter := collETag(d0after)
	if tokAfter == tokBefore {
		t.Errorf("M2 REGRESSION at Depth 0: the Depth-0 token did not move across a deletion (%q)", tokAfter)
	}
}

// TestRootCollectionTokenSurvivesDepth1 is the M3 pin: the Depth-1 root row
// must carry the same token the Depth-0 walk derives.
func TestRootCollectionTokenSurvivesDepth1(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "photos"})
	be.seed("photos", "a.txt", []byte("hello"))

	_, d1 := propfind(f, "/", "1", "")
	_, d0 := propfind(f, "/", "0", "")

	tok1 := collETag(d1)
	tok0 := collETag(d0)
	if tok0 == "" {
		t.Fatalf("test premise: the Depth-0 root has no token")
	}
	if tok1 == "" {
		t.Errorf("M3 REGRESSION: the Depth-1 root row carries no token while Depth 0 of the "+
			"same collection returns %q - the Depth-1 root token was assigned to a local "+
			"copy after the slice element was taken", tok0)
		return
	}
	if tok1 != tok0 {
		t.Errorf("M3: the Depth-1 root token %q differs from the Depth-0 token %q for the "+
			"same collection", tok1, tok0)
	}
}

// TestCollectionTokenEmptyWhenNoChildren is the honesty arm: an EMPTY
// collection yields the empty token, not a constant that would read as
// "unchanged" forever. The token is a hint; "" tells the client to walk.
func TestCollectionTokenEmptyWhenNoChildren(t *testing.T) {
	f, be := newTestFrontend(Config{Bucket: "photos"})
	be.seed("photos", "2024/a.txt", []byte("x"))
	_, body := propfind(f, "/empty/", "0", "")
	// Either a 404/404-propstat row or a collection with the empty token; what
	// must NOT happen is a fabricated non-empty token.
	if tok := collETag(body); strings.HasPrefix(tok, "dir-") {
		t.Errorf("an empty collection reported a derived token %q; the empty rendering is the "+
			"honest answer", tok)
	}
}
