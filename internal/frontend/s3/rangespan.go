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
func ParseMultiRange(header string, size int64) ([]Span, bool) {
	const unit = "bytes="
	if !strings.HasPrefix(header, unit) {
		return nil, false
	}

	spans := make([]Span, 0, 4)
	for specPart := range strings.SplitSeq(strings.TrimPrefix(header, unit), ",") {
		specPart = strings.TrimSpace(specPart)
		if specPart == "" {
			continue // tolerate empty specs around commas / whitespace
		}
		if span, ok := parseByteRangeSpec(specPart, size); ok {
			spans = append(spans, span)
		}
	}

	if len(spans) == 0 {
		return nil, false
	}
	return spans, true
}

// parseByteRangeSpec normalizes one byte-range-spec against the object
// size. It reports ok=false when the spec contributes no span, whether
// because it is malformed (wrong syntax, inverted end) or because it is
// individually unsatisfiable (start >= size, empty suffix) - the caller
// treats both as "dropped"; only a header with NO valid spec at all is
// malformed as a whole.
func parseByteRangeSpec(spec string, size int64) (Span, bool) {
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return Span{}, false // no dash at all: malformed spec
	}
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)

	switch {
	case startStr == "" && endStr == "":
		// "bytes=-" - no numbers at all: malformed spec.
		return Span{}, false
	case startStr == "":
		// Suffix form: last <endStr> bytes.
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || suffixLen < 0 {
			return Span{}, false
		}
		if suffixLen == 0 || size == 0 {
			return Span{}, false // unsatisfiable: dropped
		}
		if suffixLen > size {
			suffixLen = size // "-N" beyond EOF: whole object
		}
		return Span{Start: size - suffixLen, End: size}, true
	default:
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return Span{}, false
		}
		if start >= size {
			return Span{}, false // unsatisfiable: dropped
		}
		end := size
		if endStr != "" {
			parsedEnd, err := strconv.ParseInt(endStr, 10, 64)
			if err != nil || parsedEnd < start {
				// "5-2" (inverted) or garbage end: malformed spec.
				return Span{}, false
			}
			if parsedEnd < end {
				end = parsedEnd + 1 // half-open: inclusive end + 1
			}
		}
		return Span{Start: start, End: end}, true
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
