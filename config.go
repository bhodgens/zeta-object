package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// config.go — server configuration, credentials, and shared constants

const (
	defaultDataDir    = "./data/"
	defaultConfigFile = "config.json"
	defaultListenAddr = ":8443"
	defaultCertFile   = "certs/cert.pem"
	defaultKeyFile    = "certs/key.pem"
	defaultAccessKey  = "zetaadmin"

	// zmetad defaults (zmetad-provider-2026-09 leaf 04, Contract 4).
	// Config load owns these - the provider receives concrete values.
	// DB path per zmetad SCHEMA.md / zfs_events.h:28; binary defaults
	// to a PATH lookup per the upstream install.
	defaultZmetadDBPath = "/var/lib/zfs/zmetad.db"
	defaultZmetadBinary = "zmetad"

	// SigV4 verification region (region-config-2026-10 leaf 01).
	// Config load owns the default - the s3 frontend receives a
	// concrete value.
	defaultS3Region = "us-east-1"

	// ZFS versioning mechanism values (s3-versioning-2026-10 Contract
	// 3, extended by leaf 06). Config load owns the default
	// ("reflink" — FICLONE block-clone per-write versions, the cheapest
	// exact mechanism on ZFS 2.2+) and validates the vocabulary - the
	// s3 frontend receives a concrete value. Empty also selects the
	// default at wiring time (a zero-value struct built outside
	// loadConfig behaves like the documented default).
	defaultZfsVersioning = "reflink"

	// defaultZfsVersioningReflinkRetention is the zfs_versioning_reflink_retention
	// default: 0 = unlimited (no pruning). NOTE (leaf 07): this is now the
	// FALLBACK for buckets that do not set their own reflinkRetention;
	// per-bucket values (1, 3, 5, ...) override it per bucket.
	defaultZfsVersioningReflinkRetention = 0

	// defaultZfsBinary is the zfs_binary default: the zfs CLI resolved
	// via PATH, same convention as defaultZmetadBinary (zfs bucket
	// datasets leaf 01).
	defaultZfsBinary = "zfs"
)

// authModeNone is the opt-in zero-auth dev mode value for auth.mode
// (pluggable-authentication tree leaf 05). The behavior (DevAuthenticator)
// lives in internal/auth; this constant pins the config vocabulary.
const authModeNone = "none"

// ServerConfig holds the server configuration loaded from JSON
type ServerConfig struct {
	DataDir    string            `json:"dataDir"`    // Root directory for bucket storage
	Buckets    map[string]string `json:"buckets"`    // Custom bucket name -> path mappings
	ListenAddr string            `json:"listenAddr"` // Host:port to listen on (default ":8443")
	CertFile   string            `json:"certFile"`   // TLS certificate path (default certs/cert.pem)
	KeyFile    string            `json:"keyFile"`    // TLS private key path (default certs/key.pem)

	// Frontends selects which protocol frontends run and where they
	// listen (leaf 03). Absent/empty ⇒ [{"type":"s3"}] on the default
	// listener — exact backward compatibility.
	Frontends []FrontendConfig `json:"frontends,omitempty"`

	// Backends maps a backend type name to its construction config
	// (leaf 03; frozen JSON keys). Absent ⇒ every bucket uses the
	// default backend ("fs").
	Backends map[string]BackendCfg `json:"backends"`
	// BucketBackends records each bucket's selected backend name (the
	// object form of the buckets value). Absent/empty ⇒ default backend.
	BucketBackends map[string]string `json:"-"`

	// BucketAuditReads records each bucket's auditReads tunable (auth
	// extensions leaf 10): when true, GETs stamp a per-principal
	// user.zeta.reader.<key> breadcrumb, first read only. Absent ⇒ false
	// (zero read-path overhead — design 2b phase 1).
	BucketAuditReads map[string]bool `json:"-"`

	// BucketReflinkRetention records each bucket's reflinkRetention cap
	// (versioning leaf 07): count of version data files retained PER KEY
	// in that bucket. Absent bucket ⇒ fall back to the server-wide
	// ZfsVersioningReflinkRetention. Present 0 = keep zero version
	// copies (every overwrite discards the previous data) — the pointer
	// on the bucketCfg side distinguishes that from "not configured".
	BucketReflinkRetention map[string]int `json:"-"`

	// Identities is the multi-identity auth config (pluggable-
	// authentication tree leaf 01). Absent ⇒ only the env-pair identity
	// exists (exact pre-tree behavior).
	Identities []auth.IdentityConfig `json:"identities,omitempty"`

	// Auth carries the auth-mode switch ("": required auth — the default;
	// "none": zero-auth dev mode, loudly logged — leaf 05). Tag is a plain
	// "auth" (not omitempty): AuthConfig is a struct, so omitempty would
	// never fire and modernize flags it; "" mode remains the default.
	Auth AuthConfig `json:"auth"`

	// ZmetadDBPath / ZmetadBinary configure the zmetad events provider
	// (zmetad-provider-2026-09 leaf 04, Contract 4): the SQLite export
	// database the provider reads and the zmetad executable used for
	// `zmetad --purge`. Absent keys get defaults at config load
	// (defaultZmetadDBPath / defaultZmetadBinary) - one place owns them.
	ZmetadDBPath string `json:"zmetad_db_path"`
	ZmetadBinary string `json:"zmetad_binary"`

	// Region is the SigV4 verification region (region-config-2026-10
	// leaf 01). Absent/empty -> defaultRegion ("us-east-1") at config
	// load - one place owns the default; values are lowercased at load
	// (SigV4 regions are lowercase).
	Region string `json:"region"`

	// ZfsVersioning selects the versioning mechanism for ZFS-backed
	// buckets (s3-versioning-2026-10 Contract 3, extended by leaf 06):
	// "reflink" (FICLONE block-clone per-write versions under
	// .metadata/.versions-r/ — the default), "sidecar" (true per-write
	// versioning + delete markers as rewritten copies), "snapshots"
	// (faux versioning from the dataset's existing snapshots), or
	// "both" (reflink merged with sidecar-layout history; snapshot
	// entries stay OUT). Non-ZFS buckets always use the sidecar store
	// regardless of this key. Absent/empty -> the default at config
	// load; an unknown value aborts startup.
	ZfsVersioning string `json:"zfs_versioning"`

	// ZfsVersioningReflinkRetention caps how many reflink version DATA
	// files are retained PER KEY (newest N kept): after each recorded
	// version the OLDEST beyond the cap are pruned along with their
	// sidecar entries. 0/unset = unlimited (no pruning); a negative
	// value aborts startup. Server-wide v1 (leaf 06) — a per-bucket
	// override key is future work (README notes it).
	ZfsVersioningReflinkRetention int `json:"zfs_versioning_reflink_retention,omitempty"`

	// ZfsBucketDatasets enables ZFS bucket datasets (zfs-bucket-
	// datasets leaf 01): when true, S3 CreateBucket under dataDir
	// creates a child ZFS dataset (<dataDir dataset>/<bucket>) instead
	// of a plain directory, and DeleteBucket destroys it. Absent =>
	// false (plain directories everywhere, exact pre-feature
	// behavior). When true, startup ABORTS unless dataDir is a ZFS
	// mountpoint whose dataset name resolves (validateZfsBucketDatasets
	// in zfs_startup.go) — never a lazy first-request failure.
	ZfsBucketDatasets bool `json:"zfs_bucket_datasets"`

	// ZfsBinary is the zfs CLI binary used for all dataset operations
	// (PATH lookup, same convention as ZmetadBinary). Absent/empty ->
	// defaultZfsBinary ("zfs") at config load - one place owns it.
	ZfsBinary string `json:"zfs_binary"`

	// AuditLog configures the append-only request audit log (charter
	// exception, decided 2026-10-02). nil/absent = disabled (default off).
	AuditLog *AuditLogConfig `json:"auditLog,omitempty"`

	// bucketsErr carries a buckets-map decode failure (null/empty bucket
	// value) out of the custom UnmarshalJSON path; it is not a JSON key.
	bucketsErr error
}

// AuditLogConfig is the config.json "auditLog" block (auth extensions leaf
// 10): the append-only SigV4 request audit trail. Path empty/absent =
// disabled (default off). The writer-ONLY contract (AGENTS.md charter): no
// code path ever reads the file.
type AuditLogConfig struct {
	Path string `json:"path"`
}

// AuthConfig is the config.json "auth" block (pluggable-authentication tree
// leaf 01). Mode "" (absent) = normal auth; "none" = zero-auth dev mode
// (opt-in, loudly logged). Any other value aborts startup.
type AuthConfig struct {
	Mode string `json:"mode,omitempty"`
}

// FrontendConfig is one entry of the "frontends" config array (leaf 03).
// Type names a registered frontend factory ("s3", "webdav", "ftp", "sftp").
// ListenAddr empty = share the default listener's mux; set it to give this
// frontend its own dedicated listener. Bucket is webdav-only (single-bucket
// mode): non-empty pins that frontend's root to the named bucket. Options
// carries protocol-specific string settings for the owning factory (FTP
// passive-port range, SFTP host-key path, ...); unknown keys are rejected by
// the owning factory (fail-loud). Unknown JSON keys inside an entry abort
// startup via FrontendConfig.UnmarshalJSON (webdav-2026-09 leaf 01 Contract
// 2: fail-loud).
type FrontendConfig struct {
	Type       string            `json:"type"`
	ListenAddr string            `json:"listenAddr,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// UnmarshalJSON decodes a frontends entry with DisallowUnknownFields: a
// typo like "bogus" must abort startup, never silently drop (master
// Contract 2). Accepted keys: "type" and "listenAddr" for every frontend;
// "bucket" additionally for "webdav", "owncloud", and "h3" (the h3
// frontend pins its wrapped webdav's single bucket through it).
func (c *FrontendConfig) UnmarshalJSON(data []byte) error {
	type plain FrontendConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	cfg := FrontendConfig(p)
	if cfg.Type != "" && cfg.Type != "webdav" && cfg.Type != "owncloud" && cfg.Type != "h3" && cfg.Bucket != "" {
		return fmt.Errorf("frontend type %q does not accept the \"bucket\" key (webdav/owncloud/h3 only)", cfg.Type)
	}
	*c = cfg
	return nil
}

// BackendCfg is the per-backend-type config from config.json "backends".
// Root is the storage root; Options carries backend-specific string
// settings (fs ignores them in v1 — they exist for future backends).
type BackendCfg struct {
	Root    string            `json:"root"`
	Options map[string]string `json:"options"`
}

// bucketCfg is the JSON decoding form of a buckets map value: either the
// legacy bare string ("photos": "/mnt/photos") or the object form
// ("photos": {"path": ..., "backend": ...}).
type bucketCfg struct {
	Path       string `json:"path"`
	Backend    string `json:"backend"`
	AuditReads bool   `json:"auditReads,omitempty"`
	// ReflinkRetention is the per-bucket reflink version-retention cap
	// (versioning leaf 07): count of version data files retained PER KEY
	// in THIS bucket. Absent/nil = fall back to the server-wide
	// zfs_versioning_reflink_retention; a present value must be >= 0
	// (negative aborts startup, naming the bucket). The pointer
	// distinguishes "not configured" from an explicit 0 (= keep zero
	// version copies: every overwrite discards the previous data).
	ReflinkRetention *int `json:"reflinkRetention,omitempty"`
}

// UnmarshalJSON accepts both encodings. The legacy string form decodes to
// Path with an empty Backend (default backend at resolve time). A null
// value is a parse error naming the bucket: it would half-initialize an
// empty-path entry that later surfaces as a confusing dataDir collision
// (bughunt E6). The bucket name rides on the error via decodeBucketValue.
func (b *bucketCfg) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return &nullBucketValueError{}
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(data, &b.Path)
	}
	type plain bucketCfg
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	// Regression review (E6 follow-up): the top-level config rejects unknown
	// keys; the per-bucket object form must too, or a typo like "pth" is
	// silently dropped and the bucket silently loses its custom path.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	// ReflinkRetention must be carried across: dropping it here turned a
	// per-bucket cap into 0 (= keep ZERO version copies) while the patch
	// reported "applied", and a later Persist wrote that 0 to disk
	// (bughunt 2026-10-05 L8). The pointer is copied as-is so "not
	// configured" stays distinguishable from an explicit 0.
	b.Path, b.Backend, b.AuditReads, b.ReflinkRetention = p.Path, p.Backend, p.AuditReads, p.ReflinkRetention
	return nil
}

// nullBucketValueError marks a JSON null where a bucket value was expected;
// bucketsRaw.UnmarshalJSON wraps it with the offending bucket name.
type nullBucketValueError struct{}

func (*nullBucketValueError) Error() string {
	return "bucket value must be a path string or {\"path\":...} object, got null"
}

// bucketsRaw is the raw buckets map shape used only for JSON decoding.
type bucketsRaw map[string]bucketCfg

// UnmarshalJSON decodes either encoding of each value and fans the result
// out into the legacy Buckets (name → path) and BucketBackends
// (name → backend) fields so every existing caller of serverConfig.Buckets
// keeps its exact behavior. A null/empty bucket value surfaces as a parse
// error naming the offending bucket (bughunt E6).
func (m *bucketsRaw) UnmarshalJSON(data []byte) error {
	var raws map[string]json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	raw := make(map[string]bucketCfg, len(raws))
	for name, v := range raws {
		var bc bucketCfg
		if err := json.Unmarshal(v, &bc); err != nil {
			if strings.Contains(err.Error(), "got null") || strings.Contains(err.Error(), "nullBucketValue") {
				return fmt.Errorf("bucket %q: %w", name, err)
			}
			return fmt.Errorf("bucket %q: %w", name, err)
		}
		raw[name] = bc
	}
	*m = raw
	return nil
}

// UnmarshalJSON decodes either encoding of each buckets value and fans the
// result out into the legacy Buckets (name → path) and BucketBackends
// (name → backend) fields so every existing caller of serverConfig.Buckets
// keeps its exact behavior. A null value for a bucket is a parse error
// naming the bucket (bughunt E6).
func (m bucketsRaw) apply(cfg *ServerConfig) {
	if cfg.Buckets == nil {
		cfg.Buckets = make(map[string]string, len(m))
	}
	if len(m) == 0 {
		return
	}
	if cfg.BucketBackends == nil {
		cfg.BucketBackends = make(map[string]string)
	}
	if cfg.BucketAuditReads == nil {
		cfg.BucketAuditReads = make(map[string]bool)
	}
	if cfg.BucketReflinkRetention == nil {
		cfg.BucketReflinkRetention = make(map[string]int)
	}
	for name, bc := range m {
		if bc.Path == "" && bc.Backend == "" {
			// The bucket's UnmarshalJSON already rejected a literal null;
			// this guard catches an explicit empty object/string form that
			// would otherwise half-initialize an empty-path entry.
			cfg.bucketsErr = fmt.Errorf("bucket %q has an empty value: want a path string or {\"path\":...} object", name)
			return
		}
		cfg.Buckets[name] = bc.Path
		if bc.Backend != "" {
			cfg.BucketBackends[name] = bc.Backend
		}
		if bc.AuditReads {
			cfg.BucketAuditReads[name] = true
		}
		if bc.ReflinkRetention != nil {
			if *bc.ReflinkRetention < 0 {
				cfg.bucketsErr = fmt.Errorf("bucket %q: reflinkRetention %d is negative (0 = keep zero version copies)", name, *bc.ReflinkRetention)
				return
			}
			cfg.BucketReflinkRetention[name] = *bc.ReflinkRetention
		}
	}
}

// serverConfigAtom holds the process-wide configuration, swapped atomically
// by the reload path (SIGHUP, POST /auth/reload) while request goroutines
// read it. Readers take a consistent pointer snapshot via serverConfig().
// Runtime access goes through serverConfig(); the var name stays serverConfig
// (as a function) so call sites keep their shape — compile-time errors point
// at every former direct field read.
func serverConfig() *ServerConfig {
	return serverConfigAtom.Load()
}

var serverConfigAtom atomic.Pointer[ServerConfig]

func init() {
	serverConfigAtom.Store(&ServerConfig{
		DataDir:      defaultDataDir,
		Buckets:      make(map[string]string),
		ListenAddr:   defaultListenAddr,
		CertFile:     defaultCertFile,
		KeyFile:      defaultKeyFile,
		ZmetadDBPath: defaultZmetadDBPath,
		ZmetadBinary: defaultZmetadBinary,
	})
}

// setServerConfig is the single swap point: startup main() and loadConfig
// both publish through it. Writes stay fail-closed — the value is only
// stored after parse+normalize succeeded (loadConfig contract, unchanged).
func setServerConfig(cfg ServerConfig) {
	serverConfigAtom.Store(&cfg)
}

// configMu is retained as a writer-vs-writer latch: reloadIdentityRegistry
// can be invoked concurrently (SIGHUP goroutine AND the admin POST
// /auth/reload handler), so the whole build-validate-swap sequence runs
// under it. Readers do NOT take configMu — the atomic pointer above is the
// reader synchronization; readers always see one complete config generation.
var configMu sync.Mutex

// defaultServerConfig returns a fully-populated ServerConfig with all
// defaults applied. loadConfig always starts from a fresh copy so a failed
// parse can never leave the global partially mutated.
func defaultServerConfig() ServerConfig {
	return ServerConfig{
		DataDir:       defaultDataDir,
		Buckets:       make(map[string]string),
		ListenAddr:    defaultListenAddr,
		CertFile:      defaultCertFile,
		KeyFile:       defaultKeyFile,
		ZmetadDBPath:  defaultZmetadDBPath,
		ZmetadBinary:  defaultZmetadBinary,
		Region:        defaultS3Region,
		ZfsVersioning: defaultZfsVersioning,
		// ZfsBucketDatasets defaults to false (zero value) - plain
		// directories everywhere unless explicitly enabled.
		ZfsBinary: defaultZfsBinary,
	}
}

// loadConfig loads server configuration from a JSON file into a FRESH local
// struct; the global is only replaced once the file parsed and normalized
// successfully. A missing file keeps the defaults; any other read or parse
// error is returned to the caller (main treats it as fatal).
func loadConfig(configPath string) error {
	cfg := defaultServerConfig()

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		log.Printf("Config file %s not found, using defaults", configPath)
		configMu.Lock()
		setServerConfig(cfg)
		configMu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reading config file %s: %w", configPath, err)
	}

	if err := json.Unmarshal(stripJSON5Comments(data), &cfg); err != nil {
		return fmt.Errorf("error parsing config file %s: %w", configPath, err)
	}

	// Normalize: defaults for empty values, trailing slash on DataDir
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	} else if !strings.HasSuffix(cfg.DataDir, "/") {
		cfg.DataDir += "/"
	}
	if cfg.Buckets == nil {
		cfg.Buckets = make(map[string]string)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.CertFile == "" {
		cfg.CertFile = defaultCertFile
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = defaultKeyFile
	}
	// zmetad keys: absent/empty -> defaults (config load owns the
	// defaults; the provider receives concrete values, leaf 04).
	if cfg.ZmetadDBPath == "" {
		cfg.ZmetadDBPath = defaultZmetadDBPath
	}
	if cfg.ZmetadBinary == "" {
		cfg.ZmetadBinary = defaultZmetadBinary
	}
	// zfs_binary: absent/empty -> default (config load owns the default;
	// dataset ops receive a concrete value, zfs bucket datasets leaf 01).
	if cfg.ZfsBinary == "" {
		cfg.ZfsBinary = defaultZfsBinary
	}
	// Region: absent/empty -> default; lowercased (SigV4 regions are
	// lowercase; region-config-2026-10 leaf 01).
	cfg.Region = strings.ToLower(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = defaultS3Region
	}
	// zfs_versioning: absent/empty -> default; anything outside the
	// Contract 3 vocabulary (extended by leaf 06) is a loud startup
	// failure (s3-versioning tree Contract 3) - a typo must never
	// silently pick a mechanism.
	switch cfg.ZfsVersioning {
	case "":
		cfg.ZfsVersioning = defaultZfsVersioning
	case "snapshots", "sidecar", "reflink", "both":
	default:
		return fmt.Errorf("invalid zfs_versioning %q (want \"snapshots\", \"sidecar\", \"reflink\", or \"both\")", cfg.ZfsVersioning)
	}
	// zfs_versioning_reflink_retention: negative values abort startup
	// (fail-loud per the leaf-06 contract); 0/unset = unlimited.
	if cfg.ZfsVersioningReflinkRetention < 0 {
		return fmt.Errorf("invalid zfs_versioning_reflink_retention %d (must be >= 0; 0 = unlimited)", cfg.ZfsVersioningReflinkRetention)
	}
	// Absent/empty frontends array == S3 on the default listener (leaf 03
	// backward-compatibility rule).
	if len(cfg.Frontends) == 0 {
		cfg.Frontends = []FrontendConfig{{Type: "s3"}}
	}

	configMu.Lock()
	setServerConfig(cfg)
	configMu.Unlock()
	log.Printf("Loaded config: DataDir=%s, ListenAddr=%s, CertFile=%s, KeyFile=%s, CustomBuckets=%d",
		cfg.DataDir, cfg.ListenAddr, cfg.CertFile,
		cfg.KeyFile, len(cfg.Buckets))
	return nil
}

// UnmarshalJSON decodes a ServerConfig, accepting BOTH encodings of the
// "buckets" values (legacy bare string and the object form with an
// optional "backend" key). The object form's backend selections land in
// BucketBackends; the path form stays in the legacy Buckets field so every
// existing caller keeps its exact behavior. Unknown top-level JSON keys
// are an error (DisallowUnknownFields) so a typo like "dataDirr" fails
// loudly at startup instead of silently using the default (bughunt E6).
// A null buckets value fails with the bucket's name in the message.
func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type alias struct {
		DataDir                       string                `json:"dataDir"`
		ListenAddr                    string                `json:"listenAddr"`
		CertFile                      string                `json:"certFile"`
		KeyFile                       string                `json:"keyFile"`
		Frontends                     []FrontendConfig      `json:"frontends"`
		Backends                      map[string]BackendCfg `json:"backends"`
		Buckets                       bucketsRaw            `json:"buckets"`
		AuditLog                      *AuditLogConfig       `json:"auditLog"`
		Identities                    []auth.IdentityConfig `json:"identities"`
		Auth                          AuthConfig            `json:"auth"`
		ZmetadDBPath                  string                `json:"zmetad_db_path"`
		ZmetadBinary                  string                `json:"zmetad_binary"`
		Region                        string                `json:"region"`
		ZfsVersioning                 string                `json:"zfs_versioning"`
		ZfsVersioningReflinkRetention int                   `json:"zfs_versioning_reflink_retention"`
		ZfsBucketDatasets             bool                  `json:"zfs_bucket_datasets"`
		ZfsBinary                     string                `json:"zfs_binary"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var a alias
	if err := dec.Decode(&a); err != nil {
		return err
	}
	c.DataDir = a.DataDir
	c.ListenAddr = a.ListenAddr
	c.CertFile = a.CertFile
	c.KeyFile = a.KeyFile
	c.Frontends = a.Frontends
	c.Backends = a.Backends
	c.Identities = a.Identities
	c.Auth = a.Auth
	c.ZmetadDBPath = a.ZmetadDBPath
	c.ZmetadBinary = a.ZmetadBinary
	c.Region = a.Region
	c.ZfsVersioning = a.ZfsVersioning
	c.ZfsVersioningReflinkRetention = a.ZfsVersioningReflinkRetention
	c.ZfsBucketDatasets = a.ZfsBucketDatasets
	c.ZfsBinary = a.ZfsBinary
	c.AuditLog = a.AuditLog
	a.Buckets.apply(c)
	return c.bucketsErr
}

// buildIdentityRegistry merges the legacy env pair (always present — the
// migration contract) with the configured identities and validates the
// whole set. Fail-loud: any invalid identity, duplicate access key
// (config-vs-config or config-vs-env), or unknown auth.mode value returns
// an error naming the offender — never a silent fallback. Called from
// main() after loadCredentials; with no `identities` key the result is a
// single wildcard env identity (byte-identical to the pre-tree behavior).
func buildIdentityRegistry() (*auth.MultiRegistry, error) {
	cfg := serverConfig()
	switch cfg.Auth.Mode {
	case "", authModeNone:
	default:
		return nil, fmt.Errorf("invalid auth.mode %q (want \"\" or %q)", cfg.Auth.Mode, authModeNone)
	}
	identities := make([]auth.IdentityConfig, 0, 1+len(cfg.Identities))
	identities = append(identities, auth.EnvPair(serverCredentials.AccessKeyID, serverCredentials.SecretAccessKey))
	identities = append(identities, cfg.Identities...)
	reg, err := auth.NewMultiRegistry(identities)
	if err != nil {
		return nil, fmt.Errorf("invalid auth configuration: %w", err)
	}
	return reg, nil
}

// identityRegistry is the process-wide credential→identity registry, built
// by buildIdentityRegistry() after loadCredentials() resolves the env pair
// (pluggable-authentication tree leaf 01) and wrapped in a
// ReloadableRegistry (design-leaf 08, recommended Open Decision 1 wiring:
// wrap AT CONSTRUCTION so the env-fallback CredentialSource path rotates
// too). main() aborts on a build error, so installS3Seams only ever sees a
// valid wrapper (or nil — the pre-leaf legacy fallback). At runtime the
// SIGHUP handler swaps the wrapper's inner registry; the identityRegistry
// pointer itself never changes after startup.
var identityRegistry *auth.ReloadableRegistry

// serverConfigPath records the config file path main() resolved, so
// reloadIdentityRegistry() re-reads the SAME file (an env-var override
// must survive past startup for reload to target it).
var serverConfigPath string

// reloadIdentityRegistry re-runs the exact STARTUP sequence against the same
// config file (loadConfig → loadCredentials → buildIdentityRegistry) and
// swaps the freshly built registry into the installed ReloadableRegistry.
//
// Fail-closed: ANY error (read, parse, validate, build) leaves the OLD
// registry serving — startup's abort semantics (main.go) intentionally do
// NOT apply at reload, so a bad edit cannot take a running server down —
// and one loud line carries the validator's named-offender error. On
// success it logs the rotated-in identity NAMES only, never secrets
// (buildIdentityRegistry re-reads the env pair from the process
// environment, which is fixed; rotating the env identity therefore still
// requires a restart — documented limitation, never hidden).
func reloadIdentityRegistry() error {
	if err := loadConfig(serverConfigPath); err != nil {
		log.Printf("WARNING: auth identity reload failed, keeping previous registry: %v", err)
		return err
	}
	// The process environment is fixed, so this re-read is a no-op in
	// practice; it keeps the reload sequence byte-identical to startup.
	loadCredentials()
	reg, err := buildIdentityRegistry()
	if err != nil {
		log.Printf("WARNING: auth identity reload failed, keeping previous registry: %v", err)
		return err
	}
	identityRegistry.Swap(reg)
	names := reg.Names()
	log.Printf("Reloaded auth identities from %s: now serving %v",
		serverConfigPath, names)
	return nil
}

// Credentials store. Package-level var remains the storage, but the values
// are populated explicitly by loadCredentials() (called from main) so tests
// can vary the environment. init()-time env reads are gone.
var serverCredentials = struct {
	AccessKeyID     string
	SecretAccessKey string
}{
	AccessKeyID:     defaultAccessKey,
	SecretAccessKey: defaultAccessKey,
}

// loadCredentials reads ZETAOBJECT_ACCESS_KEY / ZETAOBJECT_SECRET_KEY. An env var
// that is SET but EMPTY is warned about and falls back to the default
// (zetaadmin) instead of silently behaving like an unset variable.
//
// Deprecated prefix: MINIS3_* remains a fallback until two minor releases
// after the zeta-object rename. ZETAOBJECT_* always wins when both are set.
func loadCredentials() {
	serverCredentials.AccessKeyID = credentialFromEnv("ZETAOBJECT_ACCESS_KEY", "MINIS3_ACCESS_KEY")
	serverCredentials.SecretAccessKey = credentialFromEnv("ZETAOBJECT_SECRET_KEY", "MINIS3_SECRET_KEY")
}

// credentialFromEnv returns the env value, the default when unset, and warns
// + defaults when the variable is set but empty. legacyKey is the pre-rename
// prefix ("") when no fallback applies.
func credentialFromEnv(key, legacyKey string) string {
	value, ok := os.LookupEnv(key)
	if !ok && legacyKey != "" {
		value, ok = os.LookupEnv(legacyKey)
		if ok {
			log.Printf("Note: %s is deprecated; set %s instead", legacyKey, key)
		}
	}
	if !ok {
		return defaultAccessKey
	}
	if value == "" {
		log.Printf("Warning: environment variable %s is set but empty; using default %q", key, defaultAccessKey)
		return defaultAccessKey
	}
	return value
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvOrDefaultLegacy is getEnvOrDefault with the deprecated MINIS3_ fallback.
// Remove alongside credentialFromEnv's legacyKey in two minor releases.
func getEnvOrDefaultLegacy(key, defaultValue string) string {
	legacyKey := strings.Replace(key, "ZETAOBJECT_", "MINIS3_", 1)
	if value := os.Getenv(key); value != "" {
		return value
	}
	if legacyKey != key {
		if value := os.Getenv(legacyKey); value != "" {
			log.Printf("Note: %s is deprecated; set %s instead", legacyKey, key)
			return value
		}
	}
	return defaultValue
}
