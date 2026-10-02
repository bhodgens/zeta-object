package objectmodel

import (
	"strings"
	"testing"
)

func TestParseTagHeader(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string
	}{
		{
			name: "valid multi-tag",
			in:   "team=infra&env=prod",
			want: map[string]string{"team": "infra", "env": "prod"},
		},
		{
			name: "URL-decoded values",
			in:   "k1=a%20b&k2=x%3Dy",
			want: map[string]string{"k1": "a b", "k2": "x=y"},
		},
		{
			name: "URL-encoded key",
			in:   "dept%2Fteam=platform",
			want: map[string]string{"dept/team": "platform"},
		},
		{
			name: "empty value allowed",
			in:   "k=",
			want: map[string]string{"k": ""},
		},
		{
			name:    "bad percent-encoding",
			in:      "k=%zz",
			wantErr: "invalid",
		},
		{
			name:    "truncated escape",
			in:      "k=%2",
			wantErr: "invalid",
		},
		{
			name:    "too many tags",
			in:      "a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9&j=10&k=11",
			wantErr: "10",
		},
		{
			name:    "aws: prefix rejected lowercase",
			in:      "aws:tag=x",
			wantErr: "aws:",
		},
		{
			name:    "AWS: prefix rejected uppercase",
			in:      "AWS:Tag=x",
			wantErr: "aws:",
		},
		{
			name:    "key too long",
			in:      strings.Repeat("k", 129) + "=v",
			wantErr: "key",
		},
		{
			name: "key exactly 128 ok",
			in:   strings.Repeat("k", 128) + "=v",
			want: map[string]string{strings.Repeat("k", 128): "v"},
		},
		{
			name:    "value too long",
			in:      "k=" + strings.Repeat("v", 257),
			wantErr: "value",
		},
		{
			name: "value exactly 256 ok",
			in:   "k=" + strings.Repeat("v", 256),
			want: map[string]string{"k": strings.Repeat("v", 256)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTagHeader(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTagHeader(%q) = %v, want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseTagHeader(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTagHeader(%q) unexpected error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseTagHeader(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("ParseTagHeader(%q)[%q] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

func TestParseTagHeaderEmpty(t *testing.T) {
	for _, in := range []string{"", "   "} {
		got, err := ParseTagHeader(in)
		if err != nil {
			t.Fatalf("ParseTagHeader(%q) unexpected error: %v", in, err)
		}
		if len(got) != 0 {
			t.Fatalf("ParseTagHeader(%q) = %v, want empty set", in, got)
		}
	}
}

func TestValidateTags(t *testing.T) {
	ok := map[string]string{
		strings.Repeat("k", 128): strings.Repeat("v", 256),
		"env":                    "prod",
	}
	if err := ValidateTags(ok); err != nil {
		t.Fatalf("ValidateTags(valid) unexpected error: %v", err)
	}

	tooMany := map[string]string{}
	for i := range 11 {
		tooMany[string(rune('a'+i))] = "v"
	}

	tests := []struct {
		name    string
		tags    map[string]string
		wantErr string
	}{
		{"too many tags", tooMany, "10"},
		{"aws: prefix", map[string]string{"AWS:x": "v"}, "aws:"},
		{"empty key", map[string]string{"": "v"}, "key"},
		{"key too long", map[string]string{strings.Repeat("k", 129): "v"}, "key"},
		{"value too long", map[string]string{"k": strings.Repeat("v", 257)}, "value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTags(tc.tags)
			if err == nil {
				t.Fatalf("ValidateTags(%v) = nil, want error containing %q", tc.tags, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateTags(%v) error = %v, want containing %q", tc.tags, err, tc.wantErr)
			}
		})
	}
}

func TestTagsToXMLFromXMLRoundTrip(t *testing.T) {
	in := map[string]string{"team": "infra", "env": "prod", "k with space": "x=y"}
	xml := TagsToXML(in)
	out, err := TagsFromXML(xml)
	if err != nil {
		t.Fatalf("TagsFromXML(TagsToXML(%v)) unexpected error: %v", in, err)
	}
	if len(out) != len(in) {
		t.Fatalf("round-trip got %v, want %v", out, in)
	}
	for k, v := range in {
		if out[k] != v {
			t.Fatalf("round-trip[%q] = %q, want %q", k, out[k], v)
		}
	}
}

func TestTagsToXMLShape(t *testing.T) {
	xml := string(TagsToXML(map[string]string{"team": "infra"}))
	for _, want := range []string{"<Tagging>", "<TagSet>", "<Tag>", "<Key>team</Key>", "<Value>infra</Value>", "</TagSet>", "</Tagging>"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("TagsToXML output missing %q:\n%s", want, xml)
		}
	}
	if got := string(TagsToXML(nil)); got != "<Tagging><TagSet></TagSet></Tagging>" {
		t.Fatalf("TagsToXML(nil) = %q, want empty TagSet wrapper", got)
	}
}

func TestTagsFromXMLErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"not XML", "this is not xml", "parse"},
		{"truncated XML", "<Tagging><TagSet><Tag><Key>k</Key>", "parse"},
		{"wrong root", "<Foo><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Foo>", "Tagging"},
		{"aws: tag in body", "<Tagging><TagSet><Tag><Key>aws:x</Key><Value>v</Value></Tag></TagSet></Tagging>", "aws:"},
		{"too many tags", func() string {
			var b strings.Builder
			b.WriteString("<Tagging><TagSet>")
			for i := range 11 {
				b.WriteString("<Tag><Key>k" + string(rune('a'+i)) + "</Key><Value>v</Value></Tag>")
			}
			b.WriteString("</TagSet></Tagging>")
			return b.String()
		}(), "10"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagsFromXML([]byte(tc.body))
			if err == nil {
				t.Fatalf("TagsFromXML(%q) = nil error, want containing %q", tc.body, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("TagsFromXML(%q) error = %v, want containing %q", tc.body, err, tc.wantErr)
			}
		})
	}
}
