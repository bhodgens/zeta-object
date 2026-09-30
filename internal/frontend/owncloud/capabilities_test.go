package owncloud

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestBuildCapabilitiesGolden pins the capabilities document byte-for-byte
// for the only v1 configuration (no metadata provider).
func TestBuildCapabilitiesGolden(t *testing.T) {
	doc := BuildCapabilities(false)
	raw, err := xml.Marshal(doc.ocPayload())
	if err != nil {
		t.Fatal(err)
	}
	want := "<version><major>10</major><minor>11</minor><micro>0</micro>" +
		"<string>10.11.0</string></version><capabilities><files></files></capabilities>"
	if string(raw) != want {
		t.Fatalf("capabilities payload =\n%s\nwant\n%s", raw, want)
	}
}

// TestCapabilitiesNeverLie: no flag zeta-object cannot honor appears in
// the document — greps the marshaled bytes for every aspirational flag
// name (master Contract 1: omit, never emit false).
func TestCapabilitiesNeverLie(t *testing.T) {
	raw, err := xml.Marshal(BuildCapabilities(false).ocPayload())
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, forbidden := range []string{
		"bigfilechunking", "undelete", "versioning", "files_sharing",
		"provisioning", "notifications", "checksums", "comments", "tags",
	} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("document contains aspirational flag %q:\n%s", forbidden, doc)
		}
	}
}

// TestBuildCapabilitiesPure: repeated calls produce identical documents
// (pure function contract).
func TestBuildCapabilitiesPure(t *testing.T) {
	a, _ := xml.Marshal(BuildCapabilities(false).ocPayload())
	b, _ := xml.Marshal(BuildCapabilities(false).ocPayload())
	if string(a) != string(b) {
		t.Fatalf("BuildCapabilities is not pure:\n%s\nvs\n%s", a, b)
	}
}
