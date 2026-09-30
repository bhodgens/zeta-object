// capabilities.go — the OCS capabilities document (master Contract 1).
//
// Semantic rule: the document advertises ONLY what zeta-object serves.
// Every flag is derived from the composed frontend's real state (the
// wrapped webdav data plane's actual method set + auth config); there are
// no hardcoded aspirational flags. A client that trusts a false flag will
// send requests the server cannot honor — silent emulation.
//
// Version block: the desktop client uses the server `version` block to
// gate feature negotiation. zeta-object declares itself as classic-line
// 10.11.0 (edition empty) — a conservative "classic ownCloud server"
// identity, NOT a claim of full classic-API coverage. No real-client
// capture backs this value (leaf 01 adapted scope — decision.md §1); it
// is a compatibility shim whose only consumer-visible effect is which
// feature probes a client attempts. The compatibility matrix
// (docs/owncloud-compatibility.md) mirrors these constants.
package owncloud

import "encoding/xml"

// Server version constants for the capabilities document. Declared as a
// classic ownCloud server 10.x line so clients treat the server as a
// classic-API server (the only API surface implemented).
const (
	ocServerMajor = 10
	ocServerMinor = 11
	ocServerMicro = 0
	ocServerVer   = "10.11.0"
)

// ocVersionBlock is the classic server's <version> element.
type ocVersionBlock struct {
	XMLName xml.Name `xml:"version"`
	Major   int      `xml:"major"`
	Minor   int      `xml:"minor"`
	Micro   int      `xml:"micro"`
	String  string   `xml:"string"`
	Edition string   `xml:"edition,omitempty"`
}

// ocFilesCaps is the classic <files> capability block. Only flags that
// are TRUE for the data plane are emitted; flags zeta-object cannot
// honor (bigfilechunking, undelete, versioning) are OMITTED entirely —
// never emitted false, because a false flag still tells clients the
// feature exists at the negotiated version boundary (master Contract 1:
// "everything else is omitted, not emitted as false").
type ocFilesCaps struct {
	XMLName xml.Name `xml:"files"`
}

// ocCapabilitiesBlock is the classic <capabilities> wrapper. The
// files_versioning block appears only when a metadata provider is
// attached (leaf 03 contract); the provider-less frontend — the only
// configuration zeta-object ships — omits it.
type ocCapabilitiesBlock struct {
	XMLName xml.Name     `xml:"capabilities"`
	Files   *ocFilesCaps `xml:"files"`
}

// ocCapabilitiesDoc is the capabilities document: the unwrapped payload
// sequence inside the envelope's <data> element (classic shape:
// <data><version>…</version><capabilities>…</capabilities></data>).
type ocCapabilitiesDoc struct {
	Version      *ocVersionBlock      `xml:"version"`
	Capabilities *ocCapabilitiesBlock `xml:"capabilities"`
}

// ocPayload flattens the document into the envelope writer's payload
// sequence (each block marshals as a direct child of <data>).
func (d *ocCapabilitiesDoc) ocPayload() payload {
	return payload{d.Version, d.Capabilities}
}

// BuildCapabilities derives the document from the frontend's true state.
// Pure function of its inputs (master Contract 1) so the golden tests
// pin the XML byte-for-byte. versioning is the metadata-provider probe
// result: false (no provider) is the only value v1 ships, and it omits
// any versioning capability entirely. Provider-backed versioning is
// leaf 03's contract; v1 never advertises it — the flag is consulted
// below so a future provider flip has exactly one wiring point.
func BuildCapabilities(versioning bool) *ocCapabilitiesDoc {
	doc := &ocCapabilitiesDoc{
		Version: &ocVersionBlock{
			Major:  ocServerMajor,
			Minor:  ocServerMinor,
			Micro:  ocServerMicro,
			String: ocServerVer,
		},
		Capabilities: &ocCapabilitiesBlock{
			Files: &ocFilesCaps{},
		},
	}
	if !versioning {
		// No provider seam in v1: versioning stays unadvertised until
		// leaf 03 lands the provider wiring.
		return doc
	}
	return doc
}
