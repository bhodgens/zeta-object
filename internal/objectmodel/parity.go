package objectmodel

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HeaderSnapshot is a protocol-neutral capture of the canonical metadata
// surface of one response, comparable with == after normalization.
type HeaderSnapshot struct {
	ContentType   string
	ContentLength string
	ETag          string
	LastModified  string
	UserMetadata  map[string]string // canonical keys (lower-case, no prefix)
}

// SnapshotHeaders builds a HeaderSnapshot via a case-insensitive header
// getter. metaKeys enumerates the raw user-metadata header names to capture
// (e.g. collected by ranging over a recorded response's Header map) — a
// getter cannot enumerate names on its own.
func SnapshotHeaders(get func(string) string, metaKeys []string) HeaderSnapshot {
	meta := ParseMetadataHeaders(metaKeys, valuesFor(get, metaKeys))
	return HeaderSnapshot{
		ContentType:   get("Content-Type"),
		ContentLength: get("Content-Length"),
		ETag:          get("ETag"),
		LastModified:  get("Last-Modified"),
		UserMetadata:  meta,
	}
}

func valuesFor(get func(string) string, keys []string) []string {
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = get(k)
	}
	return vals
}

// ParityPolicy configures the comparison AssertHeaderParityWithPolicy
// applies to the canonical header surface.
type ParityPolicy struct {
	// LastModifiedTolerance is the maximum |difference| tolerated between
	// the two Last-Modified values, parsed as HTTP-date. Zero means exact
	// string comparison. A value that fails to parse as an HTTP-date is
	// never tolerated, regardless of the tolerance setting.
	LastModifiedTolerance time.Duration
}

// DefaultParityPolicy is the pinned comparison policy for cross-backend
// header parity: every field exact, except Last-Modified, which tolerates a
// skew of up to 60 seconds when both values parse as HTTP-date (mtime
// granularity differs across backends; the canonical S3 surface must still
// agree to within one minute). AssertHeaderParity uses the zero policy
// (exact on every field) for backward compatibility.
var DefaultParityPolicy = ParityPolicy{
	LastModifiedTolerance: 60 * time.Second,
}

// AssertHeaderParity compares two snapshots field-by-field across exactly
// the canonical surface (Content-Type, Content-Length, ETag, Last-Modified,
// x-amz-meta-*) with the exact-match policy (no tolerances). This is the
// drift gate for sibling trees: same requests against FS-backed vs
// alternate-backend buckets must yield equal snapshots.
func AssertHeaderParity(a, b HeaderSnapshot) string {
	return AssertHeaderParityWithPolicy(a, b, ParityPolicy{})
}

// AssertHeaderParityWithPolicy compares two snapshots under policy. The
// ETag field is compared via ETagsMatch, so quoting and the weak-validator
// W/ prefix never read as a parity break. Last-Modified is compared exactly
// unless policy.LastModifiedTolerance > 0 and both values parse as
// HTTP-date within the tolerance. Returns an empty string on parity,
// otherwise a "\n"-joined description of every difference.
func AssertHeaderParityWithPolicy(a, b HeaderSnapshot, policy ParityPolicy) string {
	var diffs []string
	if a.ContentType != b.ContentType {
		diffs = append(diffs, fmt.Sprintf("Content-Type: %q vs %q", a.ContentType, b.ContentType))
	}
	if a.ContentLength != b.ContentLength {
		diffs = append(diffs, fmt.Sprintf("Content-Length: %q vs %q", a.ContentLength, b.ContentLength))
	}
	if !ETagsMatch(a.ETag, b.ETag) {
		diffs = append(diffs, fmt.Sprintf("ETag: %q vs %q", a.ETag, b.ETag))
	}
	if !lastModifiedMatches(a.LastModified, b.LastModified, policy.LastModifiedTolerance) {
		diffs = append(diffs, fmt.Sprintf("Last-Modified: %q vs %q", a.LastModified, b.LastModified))
	}
	for k, v := range a.UserMetadata {
		if b.UserMetadata[k] != v {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: %q vs %q", k, v, b.UserMetadata[k]))
		}
	}
	for k, v := range b.UserMetadata {
		if _, ok := a.UserMetadata[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("x-amz-meta-%s: absent vs %q", k, v))
		}
	}
	return strings.Join(diffs, "\n")
}

// lastModifiedMatches reports whether two Last-Modified header values are
// equal within tolerance. tolerance <= 0 means exact string equality.
// Values that do not parse as HTTP-date are only ever equal by exact match
// — an unparseable value never silently passes under a tolerance.
func lastModifiedMatches(a, b string, tolerance time.Duration) bool {
	if a == b {
		return true
	}
	if tolerance <= 0 {
		return false
	}
	ta, errA := http.ParseTime(a)
	tb, errB := http.ParseTime(b)
	if errA != nil || errB != nil {
		return false
	}
	delta := ta.Sub(tb)
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}
