// config_store.go — the runtime configuration store (management-api-2026-10
// leaf 02, master Contract 3).
//
// The store owns the LIVE server configuration for the life of the process;
// the startup file is a snapshot the operator may persist on demand. It
// reuses the existing fail-loud validation (ServerConfig.UnmarshalJSON's
// DisallowUnknownFields for unknown keys, buildBackendLookup for unknown
// backend names, auth.NewMultiRegistry for identity validation, and the
// zfs_versioning vocabulary) and the existing seam installers (s3_wiring.go)
// to hot-apply what has a runtime path. Everything else is recorded and
// reported as restart-required and NEVER claimed as applied.
//
// Charter: nothing here is persistent server-owned state. The store is
// memory; the config file stays the operator's own file.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// maskedSecretValue is the literal written in place of every secret the
// store hands to a caller. Pinned by test: a client echoing it back
// unchanged is a NO-OP, never an overwrite of the real secret.
const maskedSecretValue = "********"

// configFilePerm is the mode Persist writes the config document with:
// owner-only. The document carries REAL (unmasked) identity secrets — masking
// is applied on the way OUT only — so it must never be group- or
// world-readable (bughunt 2026-10-05 L9).
const configFilePerm os.FileMode = 0o600

// ConfigPatch is a partial configuration update: a JSON object whose present
// top-level keys replace the corresponding live values (absent keys are left
// untouched). Unknown keys are rejected — the same fail-loud posture as the
// startup loader (ServerConfig.UnmarshalJSON's DisallowUnknownFields).
type ConfigPatch struct {
	JSON json.RawMessage
}

// configUpdateKeys is the frozen set of top-level config keys a patch may
// carry (the JSON names ServerConfig's UnmarshalJSON accepts).
var configUpdateKeys = map[string]bool{
	"dataDir": true, "listenAddr": true, "certFile": true, "keyFile": true,
	"frontends": true, "backends": true, "buckets": true, "auditLog": true,
	"identities": true, "auth": true, "zmetad_db_path": true,
	"zmetad_binary": true, "region": true, "zfs_versioning": true,
	"zfs_versioning_reflink_retention": true, "zfs_bucket_datasets": true,
	"zfs_binary": true, "bucketSettings": true,
}

// hotApplyKeys is the pinned hot-apply set (Contract 3): the top-level keys
// with a runtime path. "buckets" is special-cased in classifyPatchKeys
// because only its per-bucket tunables (auditReads, reflinkRetention) are
// hot; a bucket layout change needs a restart. "bucketSettings" is the
// tunables-ONLY patch key (bucket name → auditReads/reflinkRetention): it
// never touches the buckets map, so it is always hot-applied and never
// restart-required.
var hotApplyKeys = map[string]bool{
	"identities":                       true,
	"region":                           true,
	"zfs_versioning":                   true,
	"zfs_versioning_reflink_retention": true,
	"zfs_bucket_datasets":              true,
	"bucketSettings":                   true,
}

// ConfigStore owns the live configuration under an RWMutex.
//
// Two configurations live here and must never be confused (bughunt
// 2026-10-05, the "restart-required keys stored as live" finding):
//
//   - live is the RUNNING configuration: exactly what the installed seams
//     serve. GET /config reports this, so it never claims a value that
//     needs a restart.
//   - desired holds a restart-required key's PENDING operator value, which
//     Persist writes so the next boot picks it up. A pending value is
//     reported under RestartRequired, never as applied and never as live.
type ConfigStore struct {
	mu              sync.RWMutex
	live            *ServerConfig
	desired         ServerConfig
	restartRequired map[string]bool
}

// configStore is the process-wide store (management-api Contract 3). Created
// at startup by initConfigStore; the management API (leaf 04) reads and
// mutates it.
var configStore *ConfigStore

// initConfigStore builds the live configuration store from the currently
// loaded server configuration. main() calls it after loadCredentials, so the
// store sees the resolved env-pair credential it masks on read.
func initConfigStore() *ConfigStore {
	configStore = NewConfigStore(serverConfig())
	return configStore
}

// NewConfigStore builds a store owning a deep copy of cfg. The copy is
// deliberate: the store must not alias caller state, so a later mutation of
// the caller's struct cannot leak into the live configuration.
func NewConfigStore(cfg *ServerConfig) *ConfigStore {
	cp := deepCopyServerConfig(cfg)
	return &ConfigStore{
		live:            &cp,
		desired:         deepCopyServerConfig(cfg),
		restartRequired: map[string]bool{},
	}
}

// Snapshot returns a deep copy of the live configuration safe to hand to a
// request handler. Every identities[].secretKey is masked, and the implicit
// env-pair identity is included (masked) so the caller never sees the
// process env credential either. The live configuration keeps the real
// secrets: masking is applied on the way OUT only.
func (s *ConfigStore) Snapshot() ServerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maskedCopyLocked()
}

// RestartRequired returns the accumulated set of keys recorded as needing a
// restart (sorted, never nil).
func (s *ConfigStore) RestartRequired() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.restartRequired))
	for k := range s.restartRequired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Apply validates and applies a partial update. On success it returns the
// applied key names (hot-applied through the existing seam installers) and
// the restart-required key names (recorded, reported, never claimed
// applied). An invalid patch changes NOTHING and returns the validator's
// error.
//
// NOTHING-CHANGES-NOTHING (bughunt 2026-10-05 M7/M8). A rejected patch must
// leave every observable surface exactly as it was:
//
//  1. VALIDATION IS SIDE-EFFECT FREE. validateCandidate used to build a real
//     identity registry, and building one WRITES the process-global rich
//     grant table internal/auth's AuthorizeOp reads - so a rejected patch
//     could flip a privilege decision. Validation now runs against a
//     side-effect-free parse (parseIdentityConfig), and the registry is built
//     only when the patch is about to commit.
//  2. HOT SEAMS ARE VALIDATED BEFORE ANY MUTATION. validateHotSeams below
//     performs every fallible installer step up front (the dataset
//     provisioner install), so a patch that cannot be installed is rejected
//     before the config view or the region seam moves.
//  3. A FAILURE AFTER A MUTATION ROLLS BACK. hotSeamsRollback re-installs
//     the RUNNING configuration, restoring the region and config view a
//     failed apply had already replaced.
//
// A restart-required key never becomes live: it lands in s.desired (what
// Persist writes for the next boot) and is reported under restartRequired,
// while s.live — what GET /config reports and what the seams serve — keeps
// the running value.
func (s *ConfigStore) Apply(patch ConfigPatch) (applied []string, restartRequired []string, err error) {
	fields, err := decodePatchFields(patch)
	if err != nil {
		return nil, nil, err
	}
	if len(fields) == 0 {
		return nil, nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Build + validate the candidate BEFORE touching any seam or live state.
	// The candidate starts from the DESIRED configuration so restart-required
	// keys compose across patches (a later patch builds on an earlier
	// pending value) while the running configuration stays put.
	candidate := deepCopyServerConfig(&s.desired)
	if err := mergePatchFields(&candidate, &s.desired, fields); err != nil {
		return nil, nil, err
	}
	if err := validateCandidate(&candidate); err != nil {
		return nil, nil, err
	}

	applied, restartRequired = classifyPatchKeys(&s.desired, &candidate, fields)

	// Validate every fallible hot-apply step BEFORE the first mutation
	// (bughunt M7: the provisioner install used to run AFTER the region and
	// config view were already installed, so a rejected patch left the live
	// SigV4 region at the rejected value while GET /config still reported
	// the old one — clients signing what the API advertised got
	// SignatureDoesNotMatch from a request reported as a no-op).
	if len(applied) > 0 {
		if err := validateHotSeams(&candidate); err != nil {
			return nil, nil, err
		}
	}

	// From here the mutations run. Any failure restores the running
	// configuration's seams instead of leaving a half-applied install.
	//
	// bughunt 2026-10-06 H1/H2: the installed seams must be built from the
	// RUNNING configuration plus the hot-applied keys, NEVER from the raw
	// candidate. The candidate also carries this patch's restart-required
	// values, so installing it wholesale put a restart-required key into the
	// live data plane while GET /config (which reads s.live) reported the old
	// one - a client reading the advertised value got a path (or a region)
	// the server was not using. merge: live + applied keys = exactly the
	// configuration the running process should serve.
	if len(applied) > 0 {
		seamCfg := deepCopyServerConfig(s.live)
		copyAppliedKeys(&seamCfg, &candidate, applied)
		if err := applyHotSeams(&seamCfg); err != nil {
			hotSeamsRollback(s.live)
			return nil, nil, err
		}
	}
	if _, ok := fields["identities"]; ok {
		// The registry is identity-scoped, not layout-scoped, and identities
		// is a hot key, so the candidate and live+applied agree here.
		if err := rebuildIdentityRegistryFor(&candidate); err != nil {
			hotSeamsRollback(s.live)
			return nil, nil, err
		}
	}

	s.desired = candidate
	// Only HOT-applied keys move the running configuration; a
	// restart-required key stays live at its running value.
	if len(applied) > 0 {
		newLive := deepCopyServerConfig(s.live)
		copyAppliedKeys(&newLive, &candidate, applied)
		s.live = &newLive
	}
	for _, k := range restartRequired {
		s.restartRequired[k] = true
	}
	return applied, restartRequired, nil
}

// copyAppliedKeys carries the hot-applied patch keys from candidate into the
// running configuration. Every key in applied has a runtime path, so the
// running configuration must match the candidate exactly for those keys;
// every other key is left at the running value (restart-required keys keep
// running until the process restarts).
func copyAppliedKeys(live, candidate *ServerConfig, applied []string) {
	for _, key := range applied {
		switch key {
		case "region":
			live.Region = candidate.Region
		case "zfs_versioning":
			live.ZfsVersioning = candidate.ZfsVersioning
		case "zfs_versioning_reflink_retention":
			live.ZfsVersioningReflinkRetention = candidate.ZfsVersioningReflinkRetention
		case "zfs_bucket_datasets":
			live.ZfsBucketDatasets = candidate.ZfsBucketDatasets
		case "zfs_binary":
			live.ZfsBinary = candidate.ZfsBinary
		case "identities":
			live.Identities = candidate.Identities
		case "buckets", "bucketSettings":
			// The buckets patch is hot only when it changed per-bucket
			// tunables; the layout maps are identical in that case.
			live.Buckets = maps.Clone(candidate.Buckets)
			live.BucketBackends = maps.Clone(candidate.BucketBackends)
			live.BucketAuditReads = maps.Clone(candidate.BucketAuditReads)
			live.BucketReflinkRetention = maps.Clone(candidate.BucketReflinkRetention)
		default:
			// A key in hotApplyKeys with no runtime effect of its own.
			log.Printf("Config store: patch key %q is hot-applied with no live-configuration field to carry", key)
		}
	}
}

// hotSeamsRollback re-installs the RUNNING configuration's hot seams after a
// failed apply, so a rejected patch leaves no seam holding a value the API
// reported as rejected. Failures are logged, never returned: the store is
// already rejecting the patch, and a rollback that itself fails must not mask
// the validator's error.
func hotSeamsRollback(live *ServerConfig) {
	if err := applyHotSeams(live); err != nil {
		log.Printf("Config store: rolling back the live seams to the running configuration failed: %v", err)
	}
}

// Persist writes the DESIRED configuration (real secrets — the file is the
// operator's own) to path atomically: temp file plus rename, the same
// technique as internal/backend/fsbackend/atomic.go:18. On any failure the
// previous file is left intact.
//
// It writes the DESIRED configuration, not the running one: a restart-required
// patch that cannot take effect until the process restarts must still reach
// the file, or the operator's change is silently lost at the next boot.
//
// The mode is owner-only (0600), never group/world-readable: masking is
// applied on the way OUT only, so this file carries REAL identity secrets
// (bughunt 2026-10-05 L9).
func (s *ConfigStore) Persist(path string) error {
	s.mu.RLock()
	data, err := marshalConfigDocument(&s.desired)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, configFilePerm)
}

// maskedCopyLocked returns a deep copy of the live configuration with all
// secrets masked. Caller holds at least a read lock.
func (s *ConfigStore) maskedCopyLocked() ServerConfig {
	cp := deepCopyServerConfig(s.live)
	// The env pair is implicit (never in the file); expose it masked so a
	// /config read shows the real set of identities without the secret.
	env := auth.EnvPair(serverCredentials.AccessKeyID, maskedSecretValue)
	identities := make([]auth.IdentityConfig, 0, 1+len(cp.Identities))
	identities = append(identities, env)
	for _, id := range cp.Identities {
		id.SecretKey = maskedSecretValue
		identities = append(identities, id)
	}
	cp.Identities = identities
	return cp
}

// decodePatchFields parses the patch's top-level keys, rejecting unknown
// ones with the same wording as the startup loader. It never returns a nil
// map alongside a nil error.
func decodePatchFields(patch ConfigPatch) (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(patch.JSON))) == 0 {
		return fields, nil
	}
	if err := json.Unmarshal(patch.JSON, &fields); err != nil {
		return nil, fmt.Errorf("invalid configuration patch: %w", err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	for key := range fields {
		if !configUpdateKeys[key] {
			return nil, fmt.Errorf("json: unknown field %q", key)
		}
	}
	return fields, nil
}

// mergePatchFields applies every present patch key onto candidate (which is
// a deep copy of live). Absent keys are untouched. Keys are processed in a
// deterministic order with "buckets" FIRST, so a "bucketSettings" patch in
// the same request lands on top of the freshly-derived tunables maps rather
// than being overwritten by mergeBuckets' map reset.
func mergePatchFields(candidate, live *ServerConfig, fields map[string]json.RawMessage) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if raw, ok := fields["buckets"]; ok {
		if err := mergePatchField(candidate, live, "buckets", raw); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if key == "buckets" {
			continue
		}
		if err := mergePatchField(candidate, live, key, fields[key]); err != nil {
			return err
		}
	}
	return nil
}

// mergePatchField dispatches one patch key: structured values decode into
// their Go shape; every scalar key is handled by mergeScalarField.
func mergePatchField(candidate, live *ServerConfig, key string, raw json.RawMessage) error {
	switch key {
	case "frontends":
		var v []FrontendConfig
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("invalid frontends: %w", err)
		}
		candidate.Frontends = v
	case "backends":
		var v map[string]BackendCfg
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("invalid backends: %w", err)
		}
		candidate.Backends = v
	case "auth":
		var v AuthConfig
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("invalid auth: %w", err)
		}
		candidate.Auth = v
	case "auditLog":
		var v *AuditLogConfig
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("invalid auditLog: %w", err)
		}
		candidate.AuditLog = v
	case "identities":
		v, err := mergeIdentities(live, raw)
		if err != nil {
			return err
		}
		candidate.Identities = v
	case "buckets":
		return mergeBuckets(candidate, raw)
	case "bucketSettings":
		return mergeBucketSettings(candidate, raw)
	default:
		return mergeScalarField(candidate, key, raw)
	}
	return nil
}

// mergeScalarField applies a scalar patch key, defaulting an empty string to
// the same default config load owns.
func mergeScalarField(candidate *ServerConfig, key string, raw json.RawMessage) error {
	switch key {
	case "dataDir":
		v, err := mergeStringField(raw, defaultDataDir, withTrailingSlash)
		if err != nil {
			return err
		}
		candidate.DataDir = v
	case "listenAddr":
		v, err := mergeStringField(raw, defaultListenAddr, nil)
		if err != nil {
			return err
		}
		candidate.ListenAddr = v
	case "certFile":
		v, err := mergeStringField(raw, defaultCertFile, nil)
		if err != nil {
			return err
		}
		candidate.CertFile = v
	case "keyFile":
		v, err := mergeStringField(raw, defaultKeyFile, nil)
		if err != nil {
			return err
		}
		candidate.KeyFile = v
	case "region":
		v, err := mergeStringField(raw, defaultS3Region, strings.ToLower)
		if err != nil {
			return err
		}
		candidate.Region = v
	case "zfs_versioning":
		v, err := mergeStringField(raw, defaultZfsVersioning, nil)
		if err != nil {
			return err
		}
		candidate.ZfsVersioning = v
	case "zfs_binary":
		v, err := mergeStringField(raw, defaultZfsBinary, nil)
		if err != nil {
			return err
		}
		candidate.ZfsBinary = v
	case "zmetad_db_path":
		v, err := mergeStringField(raw, defaultZmetadDBPath, nil)
		if err != nil {
			return err
		}
		candidate.ZmetadDBPath = v
	case "zmetad_binary":
		v, err := mergeStringField(raw, defaultZmetadBinary, nil)
		if err != nil {
			return err
		}
		candidate.ZmetadBinary = v
	case "zfs_versioning_reflink_retention":
		v, err := decodeInt(raw)
		if err != nil {
			return err
		}
		candidate.ZfsVersioningReflinkRetention = v
	case "zfs_bucket_datasets":
		v, err := decodeBool(raw)
		if err != nil {
			return err
		}
		candidate.ZfsBucketDatasets = v
	}
	return nil
}

// mergeStringField decodes a string patch value, substituting def for an
// empty value and applying normalize when set.
func mergeStringField(raw json.RawMessage, def string, normalize func(string) string) (string, error) {
	v, err := decodeString(raw)
	if err != nil {
		return "", err
	}
	if v == "" {
		v = def
	}
	if normalize != nil {
		v = normalize(v)
	}
	return v, nil
}

// withTrailingSlash mirrors config load's dataDir normalization.
func withTrailingSlash(v string) string {
	if strings.HasSuffix(v, "/") {
		return v
	}
	return v + "/"
}

// mergeBuckets replaces the bucket-derived maps from a patch's buckets value
// (either encoding, per bucketsRaw). The per-bucket tunables (auditReads,
// reflinkRetention) ride along with the object form.
func mergeBuckets(candidate *ServerConfig, raw json.RawMessage) error {
	var br bucketsRaw
	if err := json.Unmarshal(raw, &br); err != nil {
		return fmt.Errorf("invalid buckets: %w", err)
	}
	candidate.Buckets = map[string]string{}
	candidate.BucketBackends = map[string]string{}
	candidate.BucketAuditReads = map[string]bool{}
	candidate.BucketReflinkRetention = map[string]int{}
	br.apply(candidate)
	if candidate.bucketsErr != nil {
		return candidate.bucketsErr
	}
	return nil
}

// bucketSettingsPatch is the tunables-ONLY patch shape: a JSON object mapping
// bucket name → {auditReads, reflinkRetention}. Deliberately separate from
// the "buckets" object form: it populates the derived per-bucket tunable maps
// (ServerConfig.BucketAuditReads / BucketReflinkRetention) WITHOUT touching
// the config buckets map, so an auto-provisioned directory bucket stays
// auto-provisioned (not custom, still deletable through the API).
type bucketSettingsPatch struct {
	AuditReads       *bool `json:"auditReads"`
	ReflinkRetention *int  `json:"reflinkRetention"`
}

// mergeBucketSettings applies a bucketSettings patch: each named bucket's
// tunables onto candidate's derived maps. It validates the bucket name, and
// fail-loud rejects a negative reflinkRetention and any attempt to smuggle a
// path/backend through the per-bucket object (DisallowUnknownFields, the same
// posture as bucketsRaw.UnmarshalJSON). Errors name the offending bucket.
func mergeBucketSettings(candidate *ServerConfig, raw json.RawMessage) error {
	var raws map[string]json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return fmt.Errorf("invalid bucketSettings: %w", err)
	}
	if candidate.BucketAuditReads == nil {
		candidate.BucketAuditReads = make(map[string]bool, len(raws))
	}
	if candidate.BucketReflinkRetention == nil {
		candidate.BucketReflinkRetention = make(map[string]int, len(raws))
	}
	for name, v := range raws {
		if err := s3.ValidateBucketName(name); err != nil {
			return fmt.Errorf("bucketSettings bucket %q: %w", name, err)
		}
		var p bucketSettingsPatch
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("bucketSettings bucket %q: %w", name, err)
		}
		if p.ReflinkRetention != nil && *p.ReflinkRetention < 0 {
			return fmt.Errorf("bucketSettings bucket %q: reflinkRetention %d is negative (must be >= 0; 0 = keep zero version copies)", name, *p.ReflinkRetention)
		}
		if p.AuditReads != nil {
			candidate.BucketAuditReads[name] = *p.AuditReads
		}
		if p.ReflinkRetention != nil {
			candidate.BucketReflinkRetention[name] = *p.ReflinkRetention
		}
	}
	return nil
}

// mergeIdentities decodes a patch's identities array, restoring any masked
// secretKey from the live identity with the same access key (a masked value
// written back unchanged is a no-op). A masked secret for an unknown access
// key is an error — there is no real secret to preserve. A masked echo of
// the implicit env-pair identity is dropped (the env pair is always
// installed from the process environment).
func mergeIdentities(live *ServerConfig, raw json.RawMessage) ([]auth.IdentityConfig, error) {
	var incoming []auth.IdentityConfig
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("invalid identities: %w", err)
	}
	byAccessKey := make(map[string]auth.IdentityConfig, len(live.Identities))
	for _, id := range live.Identities {
		byAccessKey[id.AccessKey] = id
	}
	out := make([]auth.IdentityConfig, 0, len(incoming))
	for _, id := range incoming {
		if id.SecretKey == maskedSecretValue {
			prev, ok := byAccessKey[id.AccessKey]
			switch {
			case ok:
				id.SecretKey = prev.SecretKey
			case id.AccessKey == serverCredentials.AccessKeyID:
				// Masked echo of the implicit env identity: drop it.
				continue
			default:
				return nil, fmt.Errorf("identity %q: secretKey is masked and no existing identity has access key %q", id.Name, id.AccessKey)
			}
		}
		out = append(out, id)
	}
	return out, nil
}

// validateCandidate runs the same fail-loud checks the startup path uses.
func validateCandidate(cfg *ServerConfig) error {
	switch cfg.ZfsVersioning {
	case "snapshots", "sidecar", "reflink", "both":
	default:
		return fmt.Errorf("invalid zfs_versioning %q (want \"snapshots\", \"sidecar\", \"reflink\", or \"both\")", cfg.ZfsVersioning)
	}
	if cfg.ZfsVersioningReflinkRetention < 0 {
		return fmt.Errorf("invalid zfs_versioning_reflink_retention %d (must be >= 0; 0 = unlimited)", cfg.ZfsVersioningReflinkRetention)
	}
	if cfg.Auth.Mode != "" && cfg.Auth.Mode != authModeNone {
		return fmt.Errorf("invalid auth.mode %q (want \"\" or %q)", cfg.Auth.Mode, authModeNone)
	}
	if len(cfg.Frontends) == 0 {
		cfg.Frontends = []FrontendConfig{{Type: "s3"}}
	}
	// Unknown backend names abort, exactly as initBackendLookup does at
	// startup (eager construction; the built lookup is discarded).
	if _, err := buildBackendLookup(*cfg); err != nil {
		return err
	}
	// Identity validation reuses the registry builder (the single
	// validator): duplicate/empty access keys, grant vocabulary, ...
	//
	// Building a registry WRITES the PROCESS-GLOBAL rich-grant table that
	// internal/auth's AuthorizeOp reads (bughunt 2026-10-05 M8: a rejected
	// patch could flip a privilege decision false -> true). The build here
	// exists only to VALIDATE, so the table is captured before it and
	// restored before this returns — whether the build succeeded or failed.
	_, err := buildRegistryPreservingGrantTable(cfg, nil)
	return err
}

// identityAccessKeys is the set of access keys a registry build over cfg
// registers: the implicit env pair plus cfg's own identities. These are
// exactly the keys auth.NewMultiRegistry's WithRichGrants calls touch.
func identityAccessKeys(cfg *ServerConfig) []string {
	keys := make([]string, 0, 1+len(cfg.Identities))
	keys = append(keys, serverCredentials.AccessKeyID)
	for _, id := range cfg.Identities {
		keys = append(keys, id.AccessKey)
	}
	return keys
}

// buildRegistryPreservingGrantTable builds the registry for cfg with the
// PROCESS-GLOBAL rich-grant table restored to its exact pre-call state, and
// returns it. extra is an additional set of access keys to protect (the
// store's other configurations' identities) so no key the store knows about
// can change as a side effect of validation.
//
// Auth exposes no non-global-writing registry construction — NewMultiRegistry
// registers every identity's parsed grants through Identity.WithRichGrants,
// which writes the process-global table by design (a reload fully replaces
// it). Validation therefore restores rather than bypasses: the keys the
// build touches are known (identityAccessKeys), so restoring them is exact
// and the store never publishes an unvalidated candidate's grants.
//
// The returned registry is valid for inspection but its Identity values read
// the RESTORED global table; only rebuildIdentityRegistryFor (which restores
// nothing on success) publishes a build's grants.
func buildRegistryPreservingGrantTable(cfg *ServerConfig, extra []string) (*auth.MultiRegistry, error) {
	keys := identityAccessKeys(cfg)
	keys = append(keys, extra...)
	before := captureGrantTable(keys)

	reg, err := buildRegistryFor(cfg)

	// Unconditional restore: success or failure, the table must match its
	// pre-call state (absent keys included — WithRichGrants(nil) clears).
	restoreGrantTable(keys, before)
	return reg, err
}

// grantTableSnapshot is one access key's registered rich entries and whether
// the key had an entry at all ("absent" must be distinguishable from "no
// entries", because a cleared and an empty key are the same table state but
// a present key must be re-registered).
type grantTableSnapshot map[string][]auth.GrantExpr

// captureGrantTable reads the current process-global rich entries for keys.
func captureGrantTable(keys []string) grantTableSnapshot {
	before := make(grantTableSnapshot, len(keys))
	for _, k := range keys {
		if exprs := (auth.Identity{AccessKeyID: k}).RichGrants(); len(exprs) > 0 {
			before[k] = exprs
		}
	}
	return before
}

// restoreGrantTable re-registers exactly the captured entries, clearing any
// key that had none.
func restoreGrantTable(keys []string, before grantTableSnapshot) {
	for _, k := range keys {
		id := auth.Identity{AccessKeyID: k}
		if exprs, ok := before[k]; ok {
			id.WithRichGrants(exprs)
		} else {
			id.WithRichGrants(nil)
		}
	}
}

// validateHotSeams performs every FALLIBLE step of the hot apply BEFORE the
// first mutation (bughunt 2026-10-05 M7). applyHotSeams installs the config
// view and the SigV4 region first and installs the dataset provisioner last;
// a provisioner install that fails therefore left the live region at a value
// the API reported as rejected.
//
// The check here mirrors the provisioner installer's own argument validation
// (s3.InstallZfsDatasetProvisioner rejects an empty or unsafe parent dataset
// name and an empty zfs binary) so the common rejection happens before any
// seam moves. It is an OPTIMIZATION, not the guarantee: hotSeamsRollback is
// what makes the invariant hold for any install failure this pre-check cannot
// predict, including a step added to the installer later.
func validateHotSeams(cfg *ServerConfig) error {
	if !cfg.ZfsBucketDatasets {
		return nil
	}
	if err := s3.InstallZfsDatasetProvisioner(zfsBucketsParentDataset, cfg.ZfsBinary); err != nil {
		return err
	}
	// Undo the probe install immediately: validation must leave the live
	// seams exactly as it found them, and the real apply follows.
	s3.UninstallZfsDatasetProvisioner()
	s3.ClearZfsBucketDatasetParent()
	return nil
}

// buildRegistryFor builds the identity registry from the implicit env pair
// plus cfg's identities (the same composition buildIdentityRegistry uses at
// startup), validating them fail-loud.
func buildRegistryFor(cfg *ServerConfig) (*auth.MultiRegistry, error) {
	identities := make([]auth.IdentityConfig, 0, 1+len(cfg.Identities))
	identities = append(identities, auth.EnvPair(serverCredentials.AccessKeyID, serverCredentials.SecretAccessKey))
	identities = append(identities, cfg.Identities...)
	reg, err := auth.NewMultiRegistry(identities)
	if err != nil {
		return nil, fmt.Errorf("invalid auth configuration: %w", err)
	}
	return reg, nil
}

// rebuildIdentityRegistryFor swaps the freshly built registry into the
// installed ReloadableRegistry (the same swap the SIGHUP reload path uses).
func rebuildIdentityRegistryFor(cfg *ServerConfig) error {
	reg, err := buildRegistryFor(cfg)
	if err != nil {
		return err
	}
	if identityRegistry != nil {
		identityRegistry.Swap(reg)
	}
	return nil
}

// classifyPatchKeys splits the patch's keys into the applied (hot) and
// restart-required sets. A "buckets" patch is applied when it changes only
// per-bucket tunables and restart-required when it moves a bucket's path or
// backend.
func classifyPatchKeys(live, candidate *ServerConfig, fields map[string]json.RawMessage) (applied, restartRequired []string) {
	for key := range fields {
		switch {
		case key == "buckets":
			if bucketLayoutChanged(live, candidate) {
				restartRequired = append(restartRequired, key)
			} else {
				applied = append(applied, key)
			}
		case hotApplyKeys[key]:
			applied = append(applied, key)
		default:
			restartRequired = append(restartRequired, key)
		}
	}
	sort.Strings(applied)
	sort.Strings(restartRequired)
	return applied, restartRequired
}

// bucketLayoutChanged reports whether a patch moved any bucket's path or
// backend selection (a restart-required change).
func bucketLayoutChanged(a, b *ServerConfig) bool {
	return !maps.Equal(a.Buckets, b.Buckets) || !maps.Equal(a.BucketBackends, b.BucketBackends)
}

// decodeString / decodeInt / decodeBool decode a raw JSON scalar, rejecting
// a type mismatch.
func decodeString(raw json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("invalid value: %w", err)
	}
	return v, nil
}

func decodeInt(raw json.RawMessage) (int, error) {
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("invalid value: %w", err)
	}
	return v, nil
}

func decodeBool(raw json.RawMessage) (bool, error) {
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("invalid value: %w", err)
	}
	return v, nil
}

// deepCopyServerConfig copies a ServerConfig including every nested map,
// slice and pointer (Buckets, BucketBackends, BucketAuditReads,
// BucketReflinkRetention, Backends.Options, Frontends.Options,
// Identities grants/keys, AuditLog).
func deepCopyServerConfig(cfg *ServerConfig) ServerConfig {
	cp := *cfg
	cp.Buckets = maps.Clone(cfg.Buckets)
	cp.BucketBackends = maps.Clone(cfg.BucketBackends)
	cp.BucketAuditReads = maps.Clone(cfg.BucketAuditReads)
	cp.BucketReflinkRetention = maps.Clone(cfg.BucketReflinkRetention)
	if cfg.Backends != nil {
		cp.Backends = make(map[string]BackendCfg, len(cfg.Backends))
		for k, v := range cfg.Backends {
			bc := v
			bc.Options = maps.Clone(v.Options)
			cp.Backends[k] = bc
		}
	}
	if cfg.Frontends != nil {
		cp.Frontends = make([]FrontendConfig, len(cfg.Frontends))
		for i, f := range cfg.Frontends {
			f.Options = maps.Clone(f.Options)
			cp.Frontends[i] = f
		}
	}
	if cfg.Identities != nil {
		cp.Identities = make([]auth.IdentityConfig, len(cfg.Identities))
		for i, id := range cfg.Identities {
			if id.Grants != nil {
				grants := make(map[string]json.RawMessage, len(id.Grants))
				for k, v := range id.Grants {
					grants[k] = append(json.RawMessage(nil), v...)
				}
				id.Grants = grants
			}
			if id.SSHPublicKeys != nil {
				id.SSHPublicKeys = append([]string(nil), id.SSHPublicKeys...)
			}
			cp.Identities[i] = id
		}
	}
	if cfg.AuditLog != nil {
		al := *cfg.AuditLog
		cp.AuditLog = &al
	}
	cp.bucketsErr = nil
	return cp
}

// persistBucket is the object form of a buckets value written by Persist so
// loadConfig reconstructs every per-bucket setting (path, backend,
// auditReads, reflinkRetention).
type persistBucket struct {
	Path             string `json:"path,omitempty"`
	Backend          string `json:"backend,omitempty"`
	AuditReads       bool   `json:"auditReads,omitempty"`
	ReflinkRetention *int   `json:"reflinkRetention,omitempty"`
}

// persistableConfig mirrors the config.json key set (ServerConfig's json tags
// plus the object buckets form) so loadConfig round-trips it exactly.
type persistableConfig struct {
	DataDir                       string                   `json:"dataDir"`
	Buckets                       map[string]persistBucket `json:"buckets,omitempty"`
	ListenAddr                    string                   `json:"listenAddr"`
	CertFile                      string                   `json:"certFile"`
	KeyFile                       string                   `json:"keyFile"`
	Frontends                     []FrontendConfig         `json:"frontends,omitempty"`
	Backends                      map[string]BackendCfg    `json:"backends,omitempty"`
	Identities                    []auth.IdentityConfig    `json:"identities,omitempty"`
	Auth                          AuthConfig               `json:"auth"`
	ZmetadDBPath                  string                   `json:"zmetad_db_path"`
	ZmetadBinary                  string                   `json:"zmetad_binary"`
	Region                        string                   `json:"region"`
	ZfsVersioning                 string                   `json:"zfs_versioning"`
	ZfsVersioningReflinkRetention int                      `json:"zfs_versioning_reflink_retention,omitempty"`
	ZfsBucketDatasets             bool                     `json:"zfs_bucket_datasets,omitempty"`
	ZfsBinary                     string                   `json:"zfs_binary"`
	AuditLog                      *AuditLogConfig          `json:"auditLog,omitempty"`
}

// marshalConfigDocument renders cfg as the JSON document loadConfig reads.
func marshalConfigDocument(cfg *ServerConfig) ([]byte, error) {
	buckets := make(map[string]persistBucket, len(cfg.Buckets))
	for name, path := range cfg.Buckets {
		pb := persistBucket{
			Path:       path,
			Backend:    cfg.BucketBackends[name],
			AuditReads: cfg.BucketAuditReads[name],
		}
		if retention, ok := cfg.BucketReflinkRetention[name]; ok {
			r := retention
			pb.ReflinkRetention = &r
		}
		buckets[name] = pb
	}
	doc := persistableConfig{
		DataDir:                       cfg.DataDir,
		Buckets:                       buckets,
		ListenAddr:                    cfg.ListenAddr,
		CertFile:                      cfg.CertFile,
		KeyFile:                       cfg.KeyFile,
		Frontends:                     cfg.Frontends,
		Backends:                      cfg.Backends,
		Identities:                    cfg.Identities,
		Auth:                          cfg.Auth,
		ZmetadDBPath:                  cfg.ZmetadDBPath,
		ZmetadBinary:                  cfg.ZmetadBinary,
		Region:                        cfg.Region,
		ZfsVersioning:                 cfg.ZfsVersioning,
		ZfsVersioningReflinkRetention: cfg.ZfsVersioningReflinkRetention,
		ZfsBucketDatasets:             cfg.ZfsBucketDatasets,
		ZfsBinary:                     cfg.ZfsBinary,
		AuditLog:                      cfg.AuditLog,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling configuration: %w", err)
	}
	return data, nil
}
