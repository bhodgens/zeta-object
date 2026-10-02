package objectmodel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// legacySidecarJSON is the frozen pre-tagging sidecar byte form (fsbackend
// legacyMeta / frontend ObjectMetadata): exactly contentType, contentLength,
// eTag, customMetadata, lastModified, storagePath — no tags field.
const legacySidecarJSON = `{
  "contentType": "text/plain",
  "contentLength": 42,
  "eTag": "abc123",
  "customMetadata": {"x-amz-meta-color": "red"},
  "lastModified": "2026-10-02T00:00:00Z",
  "storagePath": "/data/bucket/a.bin"
}`

func TestLegacySidecarWithoutTagsDecodesToNilTags(t *testing.T) {
	var m LegacyObjectMetadata
	if err := json.Unmarshal([]byte(legacySidecarJSON), &m); err != nil {
		t.Fatalf("unmarshal legacy sidecar: %v", err)
	}
	if m.Tags != nil {
		t.Errorf("legacy sidecar without tags field must decode to nil Tags, got %v", m.Tags)
	}
	if m.ContentType != "text/plain" || m.ContentLength != 42 || m.ETag != "abc123" {
		t.Errorf("legacy fields wrong: %+v", m)
	}
	if m.CustomMetadata["x-amz-meta-color"] != "red" {
		t.Errorf("customMetadata wrong: %v", m.CustomMetadata)
	}
	if !m.LastModified.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("lastModified wrong: %v", m.LastModified)
	}
}

func TestLegacySidecarMarshalOmitsEmptyTags(t *testing.T) {
	for name, tags := range map[string]map[string]string{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			m := LegacyObjectMetadata{
				ContentType:   "text/plain",
				ContentLength: 1,
				ETag:          "abc123",
				CustomMetadata: map[string]string{
					"x-amz-meta-color": "red",
				},
				LastModified: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
				StoragePath:  "/data/bucket/a.bin",
				Tags:         tags,
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "tags") {
				t.Errorf("empty Tags must be omitted (byte-compat with old sidecars), got %s", raw)
			}
			// Byte-shape sanity: the six frozen keys are present.
			for _, key := range []string{"contentType", "contentLength", "eTag", "customMetadata", "lastModified", "storagePath"} {
				if !strings.Contains(string(raw), `"`+key+`"`) {
					t.Errorf("frozen sidecar key %q missing from %s", key, raw)
				}
			}
		})
	}
}

func TestLegacySidecarTagsRoundTrip(t *testing.T) {
	tags := map[string]string{"env": "prod", "team": "sre"}
	m := LegacyObjectMetadata{
		ContentType:   "application/json",
		ContentLength: 7,
		ETag:          "deadbeef",
		CustomMetadata: map[string]string{
			"x-amz-meta-owner": "alice",
		},
		LastModified: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		StoragePath:  "/data/bucket/a.json",
		Tags:         tags,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got LegacyObjectMetadata
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tags) != len(tags) {
		t.Fatalf("Tags len = %d, want %d (%s)", len(got.Tags), len(tags), raw)
	}
	for k, v := range tags {
		if got.Tags[k] != v {
			t.Errorf("Tags[%q] = %q, want %q", k, got.Tags[k], v)
		}
	}
}

func TestFromLegacyPreservesTags(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	m := LegacyObjectMetadata{
		ContentType:    "text/plain",
		ContentLength:  42,
		ETag:           "abc123",
		CustomMetadata: map[string]string{"x-amz-meta-color": "red"},
		LastModified:   now,
		Tags:           map[string]string{"env": "prod"},
	}
	o := FromLegacy("a", m)
	if len(o.Tags) != 1 || o.Tags["env"] != "prod" {
		t.Errorf("FromLegacy must preserve Tags, got %v", o.Tags)
	}
}

func TestToLegacyPreservesTags(t *testing.T) {
	o := Object{
		Key: "a", Size: 42, ETag: "abc123",
		ContentType:  "text/plain",
		LastModified: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		Metadata:     map[string]string{"color": "red"},
		Tags:         map[string]string{"env": "prod"},
	}
	m := ToLegacy(o)
	if len(m.Tags) != 1 || m.Tags["env"] != "prod" {
		t.Errorf("ToLegacy must preserve Tags, got %v", m.Tags)
	}
}

func TestLegacyTagsNilForTaglessObject(t *testing.T) {
	m := ToLegacy(Object{Key: "a"})
	if m.Tags != nil {
		t.Errorf("tagless Object must round-trip to nil Tags, got %v", m.Tags)
	}
	o := FromLegacy("a", LegacyObjectMetadata{ContentType: "text/plain"})
	if o.Tags != nil {
		t.Errorf("tagless legacy metadata must yield nil Tags, got %v", o.Tags)
	}
}

func TestResolveCopyTags(t *testing.T) {
	source := map[string]string{"env": "prod"}
	replacement := map[string]string{"env": "dev", "team": "sre"}

	tests := []struct {
		name      string
		directive string
		want      map[string]string
	}{
		{"empty directive defaults to COPY", "", source},
		{"explicit COPY", "COPY", source},
		{"copy is case-insensitive", "copy", source},
		{"REPLACE uses the new tags", "REPLACE", replacement},
		{"replace is case-insensitive", "replace", replacement},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveCopyTags(tc.directive, source, replacement)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("Tags[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}

	t.Run("COPY of nil source is nil", func(t *testing.T) {
		if got := ResolveCopyTags("COPY", nil, replacement); got != nil {
			t.Errorf("COPY with nil source must yield nil, got %v", got)
		}
	})

	t.Run("REPLACE with nil replacement is nil", func(t *testing.T) {
		if got := ResolveCopyTags("REPLACE", source, nil); got != nil {
			t.Errorf("REPLACE with nil replacement must yield nil, got %v", got)
		}
	})

	t.Run("result never aliases the source map", func(t *testing.T) {
		got := ResolveCopyTags("COPY", source, nil)
		got["env"] = "mutated"
		if source["env"] != "prod" {
			t.Errorf("mutating the result leaked into the source: %v", source)
		}
	})

	t.Run("result never aliases the replacement map", func(t *testing.T) {
		got := ResolveCopyTags("REPLACE", source, replacement)
		got["env"] = "mutated"
		if replacement["env"] != "dev" {
			t.Errorf("mutating the result leaked into the replacement: %v", replacement)
		}
	})
}
