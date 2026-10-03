package s3

import (
	"sort"
	"strconv"
	"strings"
)

// Span is one half-open byte range [Start, End) after normalization
// against the object size.
type Span struct{ Start, End int64 }

// MultiRangePartsMax is the part cap (100).
const MultiRangePartsMax = 100

// ParseMultiRange parses a Range header per RFC 9110 14.1.1/14.1.2:
// comma-separated byte-range-specs, "bytes=-N" suffix form, tolerant of
// whitespace. Returns nil, false when the header is absent/malformed
// (caller falls back to single-range or full-body paths as today).
// Malformed = no valid spec at all; individually-invalid specs are
// DROPPED per spec (an unsatisfiable span is dropped, not fatal).
// specStatus classifies one byte-range-spec failure (RFC 9110 14.2):
// syntactically uninterpretable vs interpretable-but-out-of-range.
type specStatus int

const (
	specOK            specStatus = iota
	specMalformed                 // cannot interpret: whole header ignored (200)
	specUnsatisfiable             // interpretable, out of range: candidate for 416
)

func ParseMultiRange(header string, size int64) ([]Span, bool) {
	const unit = "bytes="
	if !strings.HasPrefix(header, unit) {
		return nil, false
	}

	spans := make([]Span, 0, 4)
	anySpec := false       // at least one parseable (interpretable) spec
	anyMalformed := false  // at least one syntactically bad spec
	for specPart := range strings.SplitSeq(strings.TrimPrefix(header, unit), ",") {
		specPart = strings.TrimSpace(specPart)
		if specPart == "" {
			continue // tolerate empty specs around commas / whitespace
		}
		span, st := parseByteRangeSpec(specPart, size)
		switch st {
		case specOK:
			spans = append(spans, span)
			anySpec = true
		case specUnsatisfiable:
			anySpec = true // interpretable: makes an all-dropped header a 416
		case specMalformed:
			anyMalformed = true
		}
	}

	if len(spans) == 0 {
		// No spans survive. RFC 9110 14.2 + 14.1.1: a header that is
		// syntactically invalid is IGNORED (200 full body); a header
		// whose specs are all interpretable-but-unsatisfiable is a 416.
		// A mix (some malformed, some unsatisfiable) resolves to the
		// 416 - the client asked for ranges and none can be served.
		if anySpec && !anyMalformed {
			return nil, true
		}
		return nil, false
	}
	return spans, true
}

// parseByteRangeSpec normalizes one byte-range-spec against the object
// size. It reports ok=false when the spec contributes no span. The
// distinction ParseMultiRange needs (RFC 9110 14.2): syntactic failure
// (SpecMalformed - cannot interpret the spec at all) vs semantic failure
// (SpecUnsatisfiable - interpretable but out of range). Malformed specs
// make the whole header malformed (ignore -> 200); all-unsatisfiable
// specs make it a 416.
func parseByteRangeSpec(spec string, size int64) (Span, specStatus) {
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return Span{}, specMalformed // no dash at all: cannot interpret
	}
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)

	switch {
	case startStr == "" && endStr == "":
		// "bytes=-" - no numbers at all: cannot interpret.
		return Span{}, specMalformed
	case startStr == "":
		// Suffix form: last <endStr> bytes.
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suffixLen < 0 {
			return Span{}, specMalformed
		}
		if suffixLen == 0 || size == 0 {
			return Span{}, specUnsatisfiable
		}
		if suffixLen > size {
			suffixLen = size // "-N" beyond EOF: whole object
		}
		return Span{Start: size - suffixLen, End: size}, specOK
	default:
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return Span{}, specMalformed
		}
		if start >= size {
			return Span{}, specUnsatisfiable
		}
		end := size
		if endStr != "" {
			parsedEnd, err := strconv.ParseInt(endStr, 10, 64)
			if err != nil || parsedEnd < start {
				// "5-2" (inverted) or garbage end: malformed spec.
				return Span{}, specMalformed
			}
			if parsedEnd < end {
				end = parsedEnd + 1 // half-open: inclusive end + 1
			}
		}
		return Span{Start: start, End: end}, specOK
	}
}

// CoalesceRanges sorts spans ascending, merges overlapping/adjacent
// (next.Start <= cur.End), and enforces maxParts: when the merged count
// exceeds maxParts, return nil, false (caller serves the full body).
// Empty input -> empty slice.
func CoalesceRanges(spans []Span, maxParts int) ([]Span, bool) {
	if len(spans) == 0 {
		return []Span{}, true
	}

	sorted := make([]Span, len(spans))
	copy(sorted, spans)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	merged := sorted[:1]
	for _, next := range sorted[1:] {
		cur := &merged[len(merged)-1]
		if next.Start <= cur.End {
			if next.End > cur.End {
				cur.End = next.End
			}
			continue
		}
		merged = append(merged, next)
	}

	if len(merged) > maxParts {
		return nil, false
	}
	return merged, true
}
