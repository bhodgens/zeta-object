package objectmodel

import (
	"net/http"
	"testing"
	"time"
)

func TestParseMetadataHeaders(t *testing.T) {
	tests := []struct {
		name   string
		keys   []string
		values []string
		want   map[string]string
	}{
		{
			name:   "prefixed mixed case",
			keys:   []string{"X-Amz-Meta-Color", "x-amz-meta-Owner"},
			values: []string{"red", "alice"},
			want:   map[string]string{"color": "red", "owner": "alice"},
		},
		{
			name:   "unprefixed passthrough lowercased",
			keys:   []string{"Color"},
			values: []string{"blue"},
			want:   map[string]string{"color": "blue"},
		},
		{
			name:   "multi-value joined",
			keys:   []string{"x-amz-meta-tags"},
			values: []string{"a", "b"},
			want:   map[string]string{"tags": "a, b"},
		},
		{
			name:   "empty input",
			keys:   nil,
			values: nil,
			want:   map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMetadataHeaders(tc.keys, tc.values)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestObjectFromHeaders(t *testing.T) {
	get := func(k string) string {
		m := map[string]string{"Content-Type": "text/plain", "x-amz-meta-color": "red"}
		return m[k]
	}
	o := ObjectFromHeaders("a.txt", 5, time.Unix(1700000000, 0).UTC(),
		get, []string{"X-Amz-Meta-Color"})
	if o.Key != "a.txt" || o.Size != 5 || o.ContentType != "text/plain" {
		t.Errorf("scalar fields wrong: %+v", o)
	}
	if o.Metadata["color"] != "red" {
		t.Errorf("user metadata wrong: %v", o.Metadata)
	}
	if _, has := o.Metadata["x-amz-meta-color"]; has {
		t.Error("prefix must be stripped in canonical keys")
	}
}

func TestObjectToMetadataHeaders(t *testing.T) {
	o := Object{
		Key: "a.txt", Size: 5,
		ETag:         "d41d8cd98f00b204e9800998ecf8427e",
		LastModified: time.Unix(1700000000, 0).UTC(),
		ContentType:  "text/plain",
		Metadata:     map[string]string{"Color": "red"},
	}
	h := ObjectToMetadataHeaders(o)
	if h["Content-Type"][0] != "text/plain" {
		t.Errorf("Content-Type = %v", h["Content-Type"])
	}
	if h["Content-Length"][0] != "5" {
		t.Errorf("Content-Length = %v", h["Content-Length"])
	}
	if h["ETag"][0] != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Errorf("ETag must be quoted form, got %v", h["ETag"])
	}
	if h["Last-Modified"][0] != time.Unix(1700000000, 0).UTC().Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %v", h["Last-Modified"])
	}
	if h["x-amz-meta-color"][0] != "red" {
		t.Errorf("x-amz-meta-color = %v", h["x-amz-meta-color"])
	}
}

func TestMetadataSurfaceRoundTrip(t *testing.T) {
	want := map[string]string{"color": "red", "owner": "alice"}
	o := Object{Key: "k", Metadata: want}
	h := ObjectToMetadataHeaders(o)
	var keys, values []string
	for k, v := range h {
		keys, values = append(keys, k), append(values, v[0])
	}
	got := ParseMetadataHeaders(keys, values)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("round-trip %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestFromLegacy(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	m := LegacyObjectMetadata{
		ContentType:    "application/json",
		ContentLength:  42,
		ETag:           "abc123",
		CustomMetadata: map[string]string{"x-amz-meta-color": "red", "plain": "blue"},
		LastModified:   now,
		StoragePath:    "/data/bucket/.metadata/a.meta", // must be dropped
	}
	o := FromLegacy("a", m)
	if o.Key != "a" || o.Size != 42 || o.ContentType != "application/json" || o.ETag != "abc123" {
		t.Errorf("scalar mapping wrong: %+v", o)
	}
	if !o.LastModified.Equal(now) {
		t.Errorf("LastModified = %v, want %v", o.LastModified, now)
	}
	if o.Metadata["color"] != "red" {
		t.Errorf("prefixed legacy key must be normalized: %v", o.Metadata)
	}
	if o.Metadata["plain"] != "blue" {
		t.Errorf("unprefixed legacy key must be lower-cased and kept: %v", o.Metadata)
	}
}

func TestToLegacyDropsStoragePath(t *testing.T) {
	o := Object{Key: "a", Size: 42, ETag: `"abc123"`, ContentType: "text/plain",
		LastModified: time.Unix(1700000000, 0).UTC(),
		Metadata:     map[string]string{"color": "red"}}
	m := ToLegacy(o)
	if m.ContentLength != 42 || m.ContentType != "text/plain" || m.ETag != `"abc123"` {
		t.Errorf("mapping wrong: %+v", m)
	}
	if m.StoragePath != "" {
		t.Errorf("StoragePath must stay zero from neutral model (backend sets it), got %q", m.StoragePath)
	}
	if m.CustomMetadata["x-amz-meta-color"] != "red" {
		t.Errorf("ToLegacy must re-add prefix (legacy on-disk form): %v", m.CustomMetadata)
	}
}
