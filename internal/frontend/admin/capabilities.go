// capabilities.go — GET /capabilities, Contract 4 of the appliance parent
// plan (accessstor appliance tree leaf 03, Task 3): the per-dataset
// protocol/feature matrix plus the auth surface flags, assembled from LIVE
// sources only — the backend dataset listing, the configured frontends, the
// zfs-events provider, and the config. No cached state, no new store.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
)

// CapabilityDeps is the injected live-source surface for /capabilities.
// All fields may be nil where noted; nils render as honest false/empty
// values, never as invented capability claims.
type CapabilityDeps struct {
	// ListDatasets returns the dataset names from the backend/filesystem
	// (leaf contract: derive from the MetadataProvider/backend listing, not
	// from any new store). nil = an empty dataset list.
	ListDatasets func(ctx context.Context) ([]string, error)
	// ProtocolFlags maps one dataset name to its per-protocol availability.
	// nil = every protocol false for every dataset.
	ProtocolFlags func(ctx context.Context, dataset string) ProtocolFlags
	// FeatureFlags maps one dataset name to its ZFS-property-derived
	// feature flags (test doubles allowed by the leaf spec). nil = every
	// feature false.
	FeatureFlags func(ctx context.Context, dataset string) FeatureFlags
	// AuthFlags reports the auth surface from configuration. nil = all
	// false.
	AuthFlags func() AuthFlags
}

// ProtocolFlags is the per-dataset protocol exposure (Contract 4 shape).
type ProtocolFlags struct {
	SMB    bool `json:"smb"`
	NFS    bool `json:"nfs"`
	S3     bool `json:"s3"`
	WebDAV bool `json:"webdav"`
	SFTP   bool `json:"sftp"`
	FTP    bool `json:"ftp"`
}

// FeatureFlags is the per-dataset ZFS-property-derived feature matrix.
type FeatureFlags struct {
	Versions            bool `json:"versions"`
	Events              bool `json:"events"`
	PreviousVersionsSMB bool `json:"previous_versions_smb"`
}

// AuthFlags is the configured auth surface (Contract 4 shape).
type AuthFlags struct {
	OIDC        bool `json:"oidc"`
	SAML        bool `json:"saml"`
	STSExchange bool `json:"sts_exchange"`
	Kerberos    bool `json:"kerberos"`
}

// CapabilitiesDocument is the GET /capabilities payload.
type CapabilitiesDocument struct {
	Datasets []DatasetCapabilities `json:"datasets"`
	Auth     AuthFlags             `json:"auth"`
}

// DatasetCapabilities is one row of the matrix.
type DatasetCapabilities struct {
	Name      string        `json:"name"`
	Protocols ProtocolFlags `json:"protocols"`
	Features  FeatureFlags  `json:"features"`
}

// noCacheHeaders pin the no-store posture: the matrix is recomputed per
// request from live sources and must never be served from a cache.
func noCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
}

// handleCapabilities serves GET /capabilities. It runs behind fedAuth: an
// mTLS principal OR a valid fed_session is required, everything else is a
// 401 rendered by the wrapper.
func (f *adminFrontend) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	noCacheHeaders(w)
	deps := f.capabilities
	var doc CapabilitiesDocument
	doc.Datasets = []DatasetCapabilities{}
	if deps == nil {
		// No live-source wiring: an empty matrix with a false auth surface
		// is the honest report (the endpoint exists, nothing is claimed).
		writeJSON(w, http.StatusOK, doc)
		return
	}
	if deps.AuthFlags != nil {
		doc.Auth = deps.AuthFlags()
	}
	if deps.ListDatasets != nil {
		names, err := deps.ListDatasets(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError", "dataset listing failed")
			return
		}
		sort.Strings(names)
		for _, name := range names {
			row := DatasetCapabilities{Name: name}
			if deps.ProtocolFlags != nil {
				row.Protocols = deps.ProtocolFlags(r.Context(), name)
			}
			if deps.FeatureFlags != nil {
				row.Features = deps.FeatureFlags(r.Context(), name)
			}
			doc.Datasets = append(doc.Datasets, row)
		}
	}
	writeJSON(w, http.StatusOK, doc)
}

// marshalCapabilities is test-visible parity for the wire shape (the
// handler marshals through writeJSON; this exists so the test can assert
// the exact document struct round-trips).
func marshalCapabilities(doc CapabilitiesDocument) (json.RawMessage, error) {
	out, err := json.Marshal(doc)
	return json.RawMessage(out), err
}

var _ = marshalCapabilities // referenced only by tests today
