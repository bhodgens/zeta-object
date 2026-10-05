// richgrants.go — rich grant expressions (auth extensions leaf 09): grant
// entries scoped by key prefix, operation, and optional time window, parsed
// fail-loud at config load and enforced in exactly ONE place (AuthorizeOp).
//
// The frozen v1 shapes (Identity{AccessKeyID, BucketGrants}, Grant{Read,
// Write}) are untouched. The frozen map holds the SAFE FLOOR (what v1
// surfaces — ListBuckets filtering, SFTP CriticalOptions round-trip,
// CanRead/CanWrite pre-filters — may answer), and the rich table lives
// BESIDE it, keyed by access key: the Identity struct cannot carry it, so
// WithRichGrants registers the parsed entries under the identity's
// AccessKeyID and RichGrants reads them back. The registry build calls
// WithRichGrants for EVERY identity (nil clears any stale table), so a
// config reload fully replaces the side table — same bounded-staleness
// property as the handshake-time auth itself.
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Op is a grant operation. The vocabulary is deliberately the S3 verb
// families the frontends already dispatch on; frontends map wire verbs
// (S3 method+subresource, FTP/SFTP commands) onto these in ONE adapter
// table each.
type Op string

const (
	OpRead   Op = "read"   // GET object (+ HEAD); GET bucket sub-resources that read data
	OpWrite  Op = "write"  // PUT/POST object data, multipart upload parts
	OpList   Op = "list"   // listing operations (ListObjects*, ListMultipartUploads)
	OpDelete Op = "delete" // DELETE object/batch-delete/abort-multipart
	OpCreate Op = "create" // CreateBucket (+ initiate multipart, PUT bucket sub-resources)

	// OpAdmin is the MANAGEMENT operation value (management-api-2026-10
	// leaf 04, master Contract 6): it labels audit records written by the
	// authenticated admin frontend. It is a valid Op but is NEVER a bucket
	// grant: administrative authority comes from the client certificate,
	// never from BucketGrants. AuthorizeOp returns false for it even for a
	// full wildcard bucket grant, and ParseGrantValue rejects it (see
	// grantableOps).
	OpAdmin Op = "admin"
)

// opVocabulary is the frozen set of valid Op values. Anything else fails
// loud at config load and fail-closed at enforcement. OpAdmin is present so
// the audit record's op field has a named value; it is deliberately absent
// from grantableOps below.
var opVocabulary = map[Op]bool{
	OpRead:   true,
	OpWrite:  true,
	OpList:   true,
	OpDelete: true,
	OpCreate: true,
	OpAdmin:  true,
}

// grantableOps is the set of ops a BucketGrants entry may name. It is the
// five S3 verb ops ONLY: a bucket grant must never confer administrative
// authority, so OpAdmin is rejected at parse time (byte-identical error
// wording to the pre-leaf vocabulary) as well as at decision time.
var grantableOps = map[Op]bool{
	OpRead:   true,
	OpWrite:  true,
	OpList:   true,
	OpDelete: true,
	OpCreate: true,
}

// GrantExpr is one parsed grant entry: a key pattern (bucket or
// bucket/prefix-pattern) plus scoping.
type GrantExpr struct {
	Pattern   string // as written in config, e.g. "photos", "photos/2024/*", "*"
	Ops       map[Op]bool
	NotBefore time.Time // zero = unbounded
	NotAfter  time.Time // zero = unbounded
}

// richTable is the per-access-key side table carrying the parsed rich
// entries the frozen Identity struct cannot hold. Guarded by RWMutex:
// Lookups run per request; writes happen only at registry build/reload.
var (
	richTableMu sync.RWMutex
	richTable   = map[string][]GrantExpr{}
)

// RichGrants returns the parsed rich entries for this identity; nil for
// identities built without any object-form grant (env pair, legacy-only
// configs, dev mode, SFTP CriticalOptions round-trip) — nil ⇒ pure v1
// behavior, byte-identical. The returned slice is shared state: callers
// must not mutate it.
func (id Identity) RichGrants() []GrantExpr {
	richTableMu.RLock()
	defer richTableMu.RUnlock()
	return richTable[id.AccessKeyID]
}

// WithRichGrants returns a copy of id carrying the rich table (build-time
// composition; Identity stays a value type). A nil/empty exprs CLEARS any
// table registered under this access key — the registry build calls this
// unconditionally so a reload fully replaces the side table and a revoked
// or downgraded identity never keeps stale rich grants.
func (id Identity) WithRichGrants(exprs []GrantExpr) Identity {
	richTableMu.Lock()
	if len(exprs) == 0 {
		delete(richTable, id.AccessKeyID)
	} else {
		richTable[id.AccessKeyID] = exprs
	}
	richTableMu.Unlock()
	return id
}

// ParseGrantValue decodes ONE grants map value (string or object form)
// against the identity name for error messages. Fail-loud: unknown op,
// not-after <= not-before, unparsable RFC 3339, empty ops array, unknown
// JSON keys → error naming the offender.
func ParseGrantValue(identity string, key string, raw json.RawMessage) (GrantExpr, error) {
	if err := validateGrantPattern(identity, key); err != nil {
		return GrantExpr{}, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return GrantExpr{}, fmt.Errorf("identity %q: grant %q: empty value", identity, key)
	}
	switch trimmed[0] {
	case '"':
		// LEGACY (frozen): string value. Same frozen vocabulary and the
		// same error message shape as the pre-leaf parser.
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return GrantExpr{}, fmt.Errorf("identity %q: grant %q: %w", identity, key, err)
		}
		switch s {
		case GrantReadOnly:
			return GrantExpr{Pattern: key, Ops: map[Op]bool{OpRead: true, OpList: true}}, nil
		case GrantReadWrite:
			return GrantExpr{Pattern: key, Ops: map[Op]bool{
				OpRead: true, OpWrite: true, OpList: true, OpDelete: true, OpCreate: true,
			}}, nil
		default:
			return GrantExpr{}, fmt.Errorf("identity %q: unknown grant value %q for bucket %q (want %q or %q)",
				identity, s, key, GrantReadOnly, GrantReadWrite)
		}
	case '{':
		// NEW (object form). Unknown JSON keys fail loud (same contract as
		// the server config's DisallowUnknownFields).
		var obj struct {
			Ops       []string `json:"ops"`
			NotBefore string   `json:"not-before"`
			NotAfter  string   `json:"not-after"`
		}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			return GrantExpr{}, fmt.Errorf("identity %q: grant %q: %w", identity, key, err)
		}
		if len(obj.Ops) == 0 {
			return GrantExpr{}, fmt.Errorf("identity %q: grant %q: ops must be a non-empty array", identity, key)
		}
		ops := make(map[Op]bool, len(obj.Ops))
		for _, o := range obj.Ops {
			op := Op(o)
			if !grantableOps[op] {
				return GrantExpr{}, fmt.Errorf("identity %q: grant %q: unknown op %q (want read|write|list|delete|create)",
					identity, key, o)
			}
			ops[op] = true
		}
		var nb, na time.Time
		if obj.NotBefore != "" {
			t, err := time.Parse(time.RFC3339, obj.NotBefore)
			if err != nil {
				return GrantExpr{}, fmt.Errorf("identity %q: grant %q: not-before %q is not RFC 3339: %w",
					identity, key, obj.NotBefore, err)
			}
			nb = t
		}
		if obj.NotAfter != "" {
			t, err := time.Parse(time.RFC3339, obj.NotAfter)
			if err != nil {
				return GrantExpr{}, fmt.Errorf("identity %q: grant %q: not-after %q is not RFC 3339: %w",
					identity, key, obj.NotAfter, err)
			}
			na = t
		}
		if !nb.IsZero() && !na.IsZero() && !na.After(nb) {
			return GrantExpr{}, fmt.Errorf("identity %q: grant %q: not-after %s must be after not-before %s",
				identity, key, obj.NotAfter, obj.NotBefore)
		}
		return GrantExpr{Pattern: key, Ops: ops, NotBefore: nb, NotAfter: na}, nil
	default:
		return GrantExpr{}, fmt.Errorf("identity %q: grant %q: value must be a string (%q/%q) or an object",
			identity, key, GrantReadOnly, GrantReadWrite)
	}
}

// validateGrantPattern enforces the pattern grammar (fail-loud at load):
//
//	"*"                    wildcard bucket
//	"<bucket>"             exact bucket
//	"<bucket>/<prefix>*"   prefix-scoped; the trailing "*" is the ONLY
//	                       wildcard character (no mid-string globs, no "**")
//
// The bucket component must be non-empty and free of "," / "=" (the SFTP
// CriticalOptions round-trip serializes bucket=value pairs; a bucket name
// containing either would round-trip as a wider grant).
func validateGrantPattern(identity, key string) error {
	if key == "" {
		return fmt.Errorf("identity %q: grant bucket name must not be empty", identity)
	}
	if key == "*" {
		return nil
	}
	bucket := key
	if before, _, ok := strings.Cut(key, "/"); ok {
		bucket = before
		if bucket == "" {
			return fmt.Errorf("identity %q: grant key %q: bucket component must not be empty", identity, key)
		}
		// Slash form: the ONLY legal shape is "<bucket>/<prefix>*" with
		// exactly one trailing "*" ("bkt/pfx*" or "bkt/*").
		if !strings.HasSuffix(key, "*") {
			return fmt.Errorf("identity %q: grant key %q must be \"*\", \"<bucket>\", or \"<bucket>/<prefix>*\" (trailing star only)", identity, key)
		}
		prefix := key[:len(key)-1] // everything before the trailing star
		if strings.Contains(prefix, "*") {
			return fmt.Errorf("identity %q: grant key %q: the trailing \"*\" is the only wildcard character", identity, key)
		}
	} else if strings.Contains(key, "*") {
		// Star inside a bare bucket name is not in the grammar.
		return fmt.Errorf("identity %q: grant key %q must be \"*\", \"<bucket>\", or \"<bucket>/<prefix>*\" (trailing star only)", identity, key)
	}
	if strings.ContainsAny(bucket, ",=") {
		return fmt.Errorf("identity %q: grant bucket name %q must not contain %q or %q",
			identity, bucket, ",", "=")
	}
	return nil
}

// patternBucket returns the bucket component of a grant pattern ("*" for
// the wildcard). Patterns are load-validated, so the shape is trusted.
func patternBucket(pattern string) string {
	if pattern == "*" {
		return "*"
	}
	if before, _, ok := strings.Cut(pattern, "/"); ok {
		return before
	}
	return pattern
}

// MatchesObject reports whether the entry's pattern covers an object key
// inside bucket. Pattern rules (fail-loud at load, cheap at request time):
//
//	"*"                    → any key in any bucket (v1 wildcard)
//	"<bucket>"             → any key in that bucket (v1 exact bucket)
//	"<bucket>/<prefix>*"   → keys under the prefix; the trailing "*" is the
//	                         ONLY wildcard character, and the boundary is
//	                         '/': "photos/2024/*" matches "photos/2024/a.jpg"
//	                         but NOT "photos/20240/x" and NOT "photos/2024"
//
// A bucket-level query (key == "", i.e. listings) matches on the bucket
// component alone — the prefix suffix is irrelevant for listing the bucket
// (the documented v1-filtered-list posture; key-level visibility inside
// listings is out of scope).
func (e GrantExpr) MatchesObject(bucket, key string) bool {
	if e.Pattern == "*" {
		return true // v1 wildcard: any key in any bucket
	}
	patBucket, keyPrefix, isPrefix := splitPattern(e.Pattern)
	if bucket != patBucket {
		return false
	}
	if !isPrefix {
		return true // exact bucket: any key
	}
	if key == "" {
		return true // bucket-level (listing) query: bucket match suffices
	}
	if keyPrefix == "" || keyPrefix == "/" {
		return true // "<bucket>/*": every key in the bucket
	}
	if !strings.HasPrefix(key, keyPrefix) {
		return false
	}
	// '/'-boundary: a prefix not ending in '/' must be followed by '/'
	// (or match the key exactly) — "2024" must not match "20240/...".
	// A prefix ending in '/' needs no extra check ("2024/" covers
	// "2024/a.jpg" by HasPrefix alone).
	if strings.HasSuffix(keyPrefix, "/") {
		return true
	}
	return len(key) == len(keyPrefix) || key[len(keyPrefix)] == '/'
}

// splitPattern splits a validated pattern into (bucket, keyPrefix, isPrefix).
// keyPrefix is the object-key prefix WITHIN the bucket (bucket component
// stripped, trailing "*" removed).
func splitPattern(p string) (bucket, keyPrefix string, isPrefix bool) {
	if p == "*" {
		return "*", "", false
	}
	before, after, found := strings.Cut(p, "/")
	if !found {
		return p, "", false
	}
	return before, strings.TrimSuffix(after, "*"), true
}

// ActiveAt reports whether the time window (if any) contains t. Both ends
// are INCLUSIVE: not-before ≤ t ≤ not-after. Zero bounds are unbounded.
func (e GrantExpr) ActiveAt(t time.Time) bool {
	if !e.NotBefore.IsZero() && t.Before(e.NotBefore) {
		return false
	}
	if !e.NotAfter.IsZero() && t.After(e.NotAfter) {
		return false
	}
	return true
}

// Allows is the op check: e.Ops[op].
func (e GrantExpr) Allows(op Op) bool {
	return e.Ops[op]
}

// AuthorizeOp answers the full question: may id perform op on (bucket, key)
// at time now? THE single rich-grant decision.
//
//   - A nil rich table ⇒ exactly legacy semantics:
//     op read/list → id.CanRead(bucket); op write/delete/create →
//     id.CanWrite(bucket), matching what bucket-level v1 grants could
//     express.
//   - With a rich table: allowed iff SOME entry e satisfies
//     e.MatchesObject(bucket, key) && e.ActiveAt(now) && e.Allows(op).
//
// The v1 answer is a hard pre-filter only when NO rich entry covers the
// bucket: for buckets the rich table speaks about, the floor may hold the
// zero-bit marker (time-scoped entries), whose CanRead/CanWrite false is an
// artifact of the frozen two-bit map, not a denial — there the rich entries
// decide. For every other bucket the v1 answer is authoritative and a false
// always denies (the cheap pre-filter for frontend compat).
func AuthorizeOp(id Identity, op Op, bucket, key string, now time.Time) bool {
	if op == OpAdmin {
		// Administrative authority is the client certificate, never a
		// bucket grant: deny outright, even for a full wildcard grant
		// (master Contract 6).
		return false
	}
	if !opVocabulary[op] {
		return false // fail-closed on anything outside the vocabulary
	}
	rich := id.RichGrants()
	if len(rich) == 0 {
		return authorizeV1Op(id, op, bucket)
	}
	if !richTableCoversBucket(rich, bucket) {
		return authorizeV1Op(id, op, bucket)
	}
	for i := range rich {
		if rich[i].MatchesObject(bucket, key) && rich[i].ActiveAt(now) && rich[i].Allows(op) {
			return true
		}
	}
	return false
}

// authorizeV1Op is the frozen v1 mapping (nil rich table): delete/create ⇒
// CanWrite; list ⇒ CanRead; read/write map to themselves.
func authorizeV1Op(id Identity, op Op, bucket string) bool {
	switch op {
	case OpRead, OpList:
		return id.CanRead(bucket)
	case OpWrite, OpDelete, OpCreate:
		return id.CanWrite(bucket)
	}
	return false
}

// richTableCoversBucket reports whether any entry's pattern addresses
// bucket (the '*' wildcard pattern covers every bucket).
func richTableCoversBucket(rich []GrantExpr, bucket string) bool {
	for i := range rich {
		if patternBucket(rich[i].Pattern) == bucket {
			return true
		}
	}
	return false
}

// applyFloorUnion derives one rich entry's implied v1 floor and unions it
// into the frozen map (registry build time):
//
//   - an entry with NO time window and read and/or write ops contributes
//     its Read/Write bits to the entry's bucket key, so CanRead/CanWrite
//     never under-answer for a prefix/ops grant ("photos/2024/*" with
//     write contributes to "photos");
//   - a TIME-SCOPED read/write entry contributes a zero-bit marker Grant{}
//     under its bucket ONLY when no other entry already grants that bucket
//     read — a present authoritative entry that denies by default blocks
//     the absent-entry "*" fall-through from leaking v1 wildcard access
//     over a time-scoped bucket, while never granting MORE than the rich
//     table (the marker denies everywhere in v1 surfaces);
//   - entries with no read/write ops (list-only, delete-only) contribute
//     nothing.
func applyFloorUnion(grants map[string]Grant, e GrantExpr) {
	bucket := patternBucket(e.Pattern)
	hasRead, hasWrite := e.Ops[OpRead], e.Ops[OpWrite]
	if !hasRead && !hasWrite {
		return
	}
	bounded := !e.NotBefore.IsZero() || !e.NotAfter.IsZero()
	if !bounded {
		cur := grants[bucket]
		if hasRead {
			cur.Read = true
		}
		if hasWrite {
			cur.Write = true
		}
		grants[bucket] = cur
		return
	}
	// Time-scoped: zero-bit marker only when nothing already grants read
	// (a present entry stays authoritative; union order is irrelevant —
	// an unbounded entry processed later still sets its bits on the
	// existing entry).
	if cur, ok := grants[bucket]; !ok || !cur.Read {
		if _, ok := grants[bucket]; !ok {
			grants[bucket] = Grant{}
		}
	}
}
