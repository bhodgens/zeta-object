package s3

import (
	"reflect"
	"testing"
)

func TestParseMultiRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string // "" means header absent
		size   int64
		want   []Span
		wantOK bool
	}{
		// Task 1 table: presence / basic forms.
		{name: "absent header", header: "", size: 1000, want: nil, wantOK: false},
		{name: "single closed span", header: "bytes=0-99", size: 1000,
			want: []Span{{Start: 0, End: 100}}, wantOK: true},
		{name: "three spans in order", header: "bytes=0-99,200-299,400-499", size: 1000,
			want: []Span{{Start: 0, End: 100}, {Start: 200, End: 300}, {Start: 400, End: 500}},
			wantOK: true},
		{name: "whitespace around specs", header: "bytes= 0-99 , 200-299 ", size: 1000,
			want: []Span{{Start: 0, End: 100}, {Start: 200, End: 300}}, wantOK: true},
		{name: "whitespace inside spec", header: "bytes= 10 - 19", size: 1000,
			want: []Span{{Start: 10, End: 20}}, wantOK: true},
		{name: "trailing comma tolerated", header: "bytes=0-99,", size: 1000,
			want: []Span{{Start: 0, End: 100}}, wantOK: true},

		// Suffix form.
		{name: "suffix last 50", header: "bytes=-50", size: 1000,
			want: []Span{{Start: 950, End: 1000}}, wantOK: true},
		{name: "suffix length equals size", header: "bytes=-1000", size: 1000,
			want: []Span{{Start: 0, End: 1000}}, wantOK: true},
		{name: "suffix length beyond size clamps to whole object", header: "bytes=-1500", size: 1000,
			want: []Span{{Start: 0, End: 1000}}, wantOK: true},
		{name: "suffix zero dropped but other spec kept", header: "bytes=-0,0-99", size: 1000,
			want: []Span{{Start: 0, End: 100}}, wantOK: true},
		{name: "suffix zero only spec means no valid spec", header: "bytes=-0", size: 1000,
			want: nil, wantOK: false},
		{name: "suffix negative malformed alone", header: "bytes=-abc", size: 1000,
			want: nil, wantOK: false},

		// Open-ended form.
		{name: "open from 100", header: "bytes=100-", size: 1000,
			want: []Span{{Start: 100, End: 1000}}, wantOK: true},
		{name: "open at last byte", header: "bytes=999-", size: 1000,
			want: []Span{{Start: 999, End: 1000}}, wantOK: true},
		{name: "open at size dropped", header: "bytes=1000-", size: 1000,
			want: nil, wantOK: false},
		{name: "open beyond size dropped", header: "bytes=1500-", size: 1000,
			want: nil, wantOK: false},

		// Closed forms vs size.
		{name: "closed beyond size dropped", header: "bytes=1500-1600", size: 1000,
			want: nil, wantOK: false},
		{name: "closed clamps end to size", header: "bytes=900-1999", size: 1000,
			want: []Span{{Start: 900, End: 1000}}, wantOK: true},
		{name: "single byte span", header: "bytes=0-0", size: 1000,
			want: []Span{{Start: 0, End: 1}}, wantOK: true},
		{name: "last byte inclusive end", header: "bytes=999-999", size: 1000,
			want: []Span{{Start: 999, End: 1000}}, wantOK: true},
		{name: "inverted range malformed alone", header: "bytes=99-0", size: 1000,
			want: nil, wantOK: false},

		// Malformed headers.
		{name: "garbage spec", header: "bytes=abc", size: 1000, want: nil, wantOK: false},
		{name: "bare dash", header: "bytes=-", size: 1000, want: nil, wantOK: false},
		{name: "bare dash with other dropped spec", header: "bytes=-,1500-1600", size: 1000,
			want: nil, wantOK: false},
		{name: "missing unit", header: "0-99", size: 1000, want: nil, wantOK: false},
		{name: "wrong unit items", header: "items=0-5", size: 1000, want: nil, wantOK: false},
		{name: "wrong unit spec among bytes dropped", header: "bytes=0-99,items=0-5", size: 1000,
			want: []Span{{Start: 0, End: 100}}, wantOK: true},
		{name: "negative start malformed alone", header: "bytes=-1-5", size: 1000,
			want: nil, wantOK: false},
		{name: "empty unit value", header: "bytes=", size: 1000, want: nil, wantOK: false},

		// Mixed valid + invalid: keep the valid, drop the invalid.
		{name: "mixed valid invalid garbage", header: "bytes=1500-1600,0-99,abc,300-399", size: 1000,
			want: []Span{{Start: 0, End: 100}, {Start: 300, End: 400}}, wantOK: true},
		{name: "mixed all invalid", header: "bytes=1500-1600,abc,-0", size: 1000,
			want: nil, wantOK: false},
		{name: "mixed dropped open and kept span", header: "bytes=1000-,200-299", size: 1000,
			want: []Span{{Start: 200, End: 300}}, wantOK: true},

		// Zero-size objects: every spec is unsatisfiable.
		{name: "size zero closed spec", header: "bytes=0-99", size: 0, want: nil, wantOK: false},
		{name: "size zero suffix", header: "bytes=-10", size: 0, want: nil, wantOK: false},
		{name: "size zero open", header: "bytes=0-", size: 0, want: nil, wantOK: false},

		// One-byte objects.
		{name: "size one closed single byte", header: "bytes=0-0", size: 1,
			want: []Span{{Start: 0, End: 1}}, wantOK: true},
		{name: "size one open", header: "bytes=0-", size: 1,
			want: []Span{{Start: 0, End: 1}}, wantOK: true},
		{name: "size one suffix whole object", header: "bytes=-1", size: 1,
			want: []Span{{Start: 0, End: 1}}, wantOK: true},
		{name: "size one open at size dropped", header: "bytes=1-", size: 1,
			want: nil, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseMultiRange(tt.header, tt.size)
			if ok != tt.wantOK {
				t.Fatalf("ParseMultiRange(%q, %d) ok = %v, want %v", tt.header, tt.size, ok, tt.wantOK)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ParseMultiRange(%q, %d) = %v, want nil", tt.header, tt.size, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseMultiRange(%q, %d) = %v, want %v", tt.header, tt.size, got, tt.want)
			}
		})
	}
}

func TestCoalesceRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		spans  []Span
		want   []Span
		wantOK bool
	}{
		{name: "empty input", spans: []Span{}, want: []Span{}, wantOK: true},
		{name: "nil input", spans: nil, want: []Span{}, wantOK: true},
		{name: "single span unchanged", spans: []Span{{Start: 0, End: 100}},
			want: []Span{{Start: 0, End: 100}}, wantOK: true},
		{name: "unordered input sorted", spans: []Span{{Start: 400, End: 500}, {Start: 0, End: 100}, {Start: 200, End: 300}},
			want: []Span{{Start: 0, End: 100}, {Start: 200, End: 300}, {Start: 400, End: 500}}, wantOK: true},
		{name: "overlap merged", spans: []Span{{Start: 0, End: 100}, {Start: 50, End: 150}},
			want: []Span{{Start: 0, End: 150}}, wantOK: true},
		{name: "overlap reverse order merged", spans: []Span{{Start: 50, End: 150}, {Start: 0, End: 100}},
			want: []Span{{Start: 0, End: 150}}, wantOK: true},
		{name: "adjacency merged", spans: []Span{{Start: 0, End: 100}, {Start: 100, End: 200}},
			want: []Span{{Start: 0, End: 200}}, wantOK: true},
		{name: "gap preserved", spans: []Span{{Start: 0, End: 99}, {Start: 200, End: 299}},
			want: []Span{{Start: 0, End: 99}, {Start: 200, End: 299}}, wantOK: true},
		{name: "nested absorbed", spans: []Span{{Start: 0, End: 1000}, {Start: 10, End: 20}},
			want: []Span{{Start: 0, End: 1000}}, wantOK: true},
		{name: "nested reverse order absorbed", spans: []Span{{Start: 10, End: 20}, {Start: 0, End: 1000}},
			want: []Span{{Start: 0, End: 1000}}, wantOK: true},
		{name: "touching with gap not merged", spans: []Span{{Start: 0, End: 100}, {Start: 101, End: 200}},
			want: []Span{{Start: 0, End: 100}, {Start: 101, End: 200}}, wantOK: true},
		{name: "three way chain merged to one", spans: []Span{{Start: 0, End: 100}, {Start: 300, End: 400}, {Start: 100, End: 300}},
			want: []Span{{Start: 0, End: 400}}, wantOK: true},
		{name: "identical spans dedup to one", spans: []Span{{Start: 0, End: 100}, {Start: 0, End: 100}},
			want: []Span{{Start: 0, End: 100}}, wantOK: true},
		{name: "zero length span kept", spans: []Span{{Start: 5, End: 5}},
			want: []Span{{Start: 5, End: 5}}, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := CoalesceRanges(tt.spans, MultiRangePartsMax)
			if ok != tt.wantOK {
				t.Fatalf("CoalesceRanges(%v, %d) ok = %v, want %v", tt.spans, MultiRangePartsMax, ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("CoalesceRanges(%v, %d) = %v, want %v", tt.spans, MultiRangePartsMax, got, tt.want)
			}
		})
	}
}

func TestCoalesceRangesCap(t *testing.T) {
	t.Parallel()

	t.Run("at cap kept", func(t *testing.T) {
		t.Parallel()

		spans := make([]Span, 0, MultiRangePartsMax)
		for i := range MultiRangePartsMax {
			spans = append(spans, Span{Start: int64(i * 10), End: int64(i*10 + 5)})
		}
		got, ok := CoalesceRanges(spans, MultiRangePartsMax)
		if !ok {
			t.Fatalf("CoalesceRanges(100 disjoint spans, %d) ok = false, want true", MultiRangePartsMax)
		}
		if len(got) != MultiRangePartsMax {
			t.Fatalf("CoalesceRanges kept %d spans, want %d", len(got), MultiRangePartsMax)
		}
		if got[0] != (Span{Start: 0, End: 5}) || got[len(got)-1] != (Span{Start: 990, End: 995}) {
			t.Fatalf("CoalesceRanges sorted output wrong: first %v, last %v", got[0], got[len(got)-1])
		}
	})

	t.Run("over cap rejected", func(t *testing.T) {
		t.Parallel()

		spans := make([]Span, 0, 101)
		for i := range 101 {
			spans = append(spans, Span{Start: int64(i * 10), End: int64(i*10 + 5)})
		}
		got, ok := CoalesceRanges(spans, MultiRangePartsMax)
		if ok {
			t.Fatalf("CoalesceRanges(101 disjoint spans, %d) ok = true, want false", MultiRangePartsMax)
		}
		if got != nil {
			t.Fatalf("CoalesceRanges over cap = %v, want nil", got)
		}
	})

	t.Run("merging brings under cap", func(t *testing.T) {
		t.Parallel()

		// 101 input spans but every other one is adjacent, so they merge
		// down to 51 spans - under the cap, kept.
		spans := make([]Span, 0, 101)
		for i := range 101 {
			if i%2 == 0 {
				spans = append(spans, Span{Start: int64(i * 10), End: int64(i*10 + 10)})
				continue
			}
			spans = append(spans, Span{Start: int64(i * 10), End: int64(i*10 + 5)})
		}
		got, ok := CoalesceRanges(spans, MultiRangePartsMax)
		if !ok {
			t.Fatal("CoalesceRanges ok = false after merging, want true")
		}
		if len(got) != 51 {
			t.Fatalf("CoalesceRanges kept %d spans, want 51", len(got))
		}
	})

	t.Run("small custom cap", func(t *testing.T) {
		t.Parallel()

		spans := []Span{{Start: 0, End: 10}, {Start: 20, End: 30}, {Start: 40, End: 50}}
		got, ok := CoalesceRanges(spans, 2)
		if ok {
			t.Fatal("CoalesceRanges(3 spans, cap 2) ok = true, want false")
		}
		if got != nil {
			t.Fatalf("CoalesceRanges(3 spans, cap 2) = %v, want nil", got)
		}

		got, ok = CoalesceRanges(spans[:2], 2)
		if !ok || len(got) != 2 {
			t.Fatalf("CoalesceRanges(2 spans, cap 2) = %v, %v; want 2 spans, true", got, ok)
		}
	})
}

func TestSpanIsHalfOpen(t *testing.T) {
	t.Parallel()

	// Contract sanity: Span is half-open [Start, End); the parser produces
	// End = last-byte-inclusive + 1 and never End <= Start for kept spans.
	spans, ok := ParseMultiRange("bytes=10-19,20-29,30-39", 100)
	if !ok {
		t.Fatal("ParseMultiRange ok = false, want true")
	}
	want := []Span{{Start: 10, End: 20}, {Start: 20, End: 30}, {Start: 30, End: 40}}
	if !reflect.DeepEqual(spans, want) {
		t.Fatalf("ParseMultiRange = %v, want %v (adjacent half-open spans)", spans, want)
	}
	coalesced, ok := CoalesceRanges(spans, MultiRangePartsMax)
	if !ok {
		t.Fatal("CoalesceRanges ok = false, want true")
	}
	if !reflect.DeepEqual(coalesced, []Span{{Start: 10, End: 40}}) {
		t.Fatalf("CoalesceRanges adjacent half-open spans = %v, want [{10 40}]", coalesced)
	}
}
