package objectmodel

import (
	"reflect"
	"testing"
	"time"
)

func TestPinnedStructShapes(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Object", Object{Key: "a", Size: 1, ETag: `"d41d8..."`, LastModified: now,
			ContentType: "text/plain", Metadata: map[string]string{"color": "red"}},
			Object{Key: "a", Size: 1, ETag: `"d41d8..."`, LastModified: now,
				ContentType: "text/plain", Metadata: map[string]string{"color": "red"}}},
		{"BucketInfo", BucketInfo{Name: "b", CreatedAt: now}, BucketInfo{Name: "b", CreatedAt: now}},
		{"ListPage", ListPage{Objects: []Object{{Key: "a"}}, CommonPrefixes: []string{"p/"},
			IsTruncated: true, NextToken: "tok"},
			ListPage{Objects: []Object{{Key: "a"}}, CommonPrefixes: []string{"p/"},
				IsTruncated: true, NextToken: "tok"}},
		{"ListParams", ListParams{Prefix: "p/", Delimiter: "/", StartAfter: "p/x",
			ContinuationToken: "t", MaxKeys: 10},
			ListParams{Prefix: "p/", Delimiter: "/", StartAfter: "p/x",
				ContinuationToken: "t", MaxKeys: 10}},
		{"GetOptions", GetOptions{IfMatch: `"e"`, IfNoneMatch: "*", Range: "bytes=0-1"},
			GetOptions{IfMatch: `"e"`, IfNoneMatch: "*", Range: "bytes=0-1"}},
		{"PutOptions", PutOptions{ContentType: "text/plain", Metadata: map[string]string{"a": "b"},
			IfMatch: `"e"`}, PutOptions{ContentType: "text/plain", Metadata: map[string]string{"a": "b"},
			IfMatch: `"e"`}},
		{"CapabilitySet", CapabilitySet{Multipart: true, MetadataProviders: []string{"zfs"}},
			CapabilitySet{Multipart: true, MetadataProviders: []string{"zfs"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// DeepEqual, not ==: Object/ListPage/CapabilitySet contain
			// map/slice fields and are not comparable with ==.
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Errorf("%+v != %+v", tc.got, tc.want)
			}
		})
	}
}

func TestNormalizeETag(t *testing.T) {
	tests := []struct{ in, want string }{
		{`"d41d8cd98f00b204e9800998ecf8427e"`, "d41d8cd98f00b204e9800998ecf8427e"},
		{"d41d8cd98f00b204e9800998ecf8427e", "d41d8cd98f00b204e9800998ecf8427e"},
		{`""`, ""},
		{"", ""},
		{`W/"weak123"`, `W/"weak123"`}, // W/ prefix preserved verbatim; only symmetric surrounding quotes stripped
	}
	for _, tc := range tests {
		if got := NormalizeETag(tc.in); got != tc.want {
			t.Errorf("NormalizeETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestQuotedETag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"d41d8cd98f00b204e9800998ecf8427e", `"d41d8cd98f00b204e9800998ecf8427e"`},
		{`"d41d8cd98f00b204e9800998ecf8427e"`, `"d41d8cd98f00b204e9800998ecf8427e"`},
		{"", `""`},
	}
	for _, tc := range tests {
		if got := QuotedETag(tc.in); got != tc.want {
			t.Errorf("QuotedETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestETagsMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`"abc"`, "abc", true},
		{`"abc"`, `"abc"`, true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"", "", true},
	}
	for _, tc := range tests {
		if got := ETagsMatch(tc.a, tc.b); got != tc.want {
			t.Errorf("ETagsMatch(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNormalizeMetadataKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"X-Amz-Meta-Color", "color"},
		{"x-amz-meta-color", "color"},
		{"Color", "color"},
		{"color", "color"},
		{"", ""},
		{"x-amz-meta-", ""},
	}
	for _, tc := range tests {
		if got := NormalizeMetadataKey(tc.in); got != tc.want {
			t.Errorf("NormalizeMetadataKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMetadataHeaderName(t *testing.T) {
	if got, want := MetadataHeaderName("Color"), "x-amz-meta-color"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
