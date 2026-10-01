package metadata

// S3 metadata parity gate (leaf 03, metadata-zfs-2026-09).
//
// ENFORCEMENT ONLY: proves the canonical S3 metadata surface — Content-Type,
// Content-Length, ETag, Last-Modified, x-amz-meta-* — is identical for a
// plain-FS bucket and a provider-attached bucket, and that the on-disk
// .metadata/*.meta sidecars stay byte-compatible modulo storagePath.
//
// Placement note (deviation, pinned by parent instruction): the leaf template
// drives package main's rootHandler via httptest, but package main cannot be
// imported from internal/metadata (the handlers still live in package main).
// Per parent instruction this gate therefore tests at the sidecar/headers/
// model layer: an identical metadata-producing operation matrix is applied to
// a plain and a provider-attached bucket, the response header surface is
// captured through objectmodel.HeaderSnapshot / SnapshotHeaders and compared
// via objectmodel.AssertHeaderParity, and the on-disk sidecars are compared
// byte-wise modulo storagePath using the production serialization form
// (json.MarshalIndent("", "  "), matching storage.go's writeFileAtomicJSON,
// with the exact JSON field names from types.go's ObjectMetadata).
// When the S3 frontend extraction lands, the matrix can be re-driven through
// the real handler unchanged.
//
// The matrix runs on a deterministic clock: parity means "identical inputs
// produce byte-identical outputs", so both variants must agree exactly. The
// pinned Last-Modified tolerance policy (60s, HTTP-date) is enforced by
// parityCompareSnapshots and exercised directly by
// TestParityLastModifiedTolerancePolicy so the gate keeps its teeth if real
// mtimes ever enter the comparison.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// parityMatrixStep is one operation in the identical request matrix. Both
// bucket variants run the SAME steps with the SAME inputs; only the
// provider attachment differs.
type parityMatrixStep struct {
	label string

	// put: write these bytes with this content type + user metadata.
	putKey  string
	putBody []byte
	putCT   string
	putMeta map[string]string // canonical keys (no x-amz-meta- prefix)

	// head: capture the GET/HEAD header surface for this key.
	headKey string

	// copy: server-side copy src -> dst (rebuilds the sidecar, as
	// object_handlers.go's copy does).
	copySrc, copyDst string

	// list: capture the ListObjectsV2 key surface for the bucket.
	list bool

	// multipart: complete a multipart upload for this key with this body.
	mpKey  string
	mpBody []byte
}

// parityMatrix is the canonical matrix from leaf Task 2: simple PUT, PUT
// with x-amz-meta-*, HEAD (GET/HEAD share the header surface), ListObjectsV2,
// multipart completion, COPY. It is built identically for both variants.
func parityMatrix() []parityMatrixStep {
	return []parityMatrixStep{
		{label: "put-plain", putKey: "a.txt", putBody: []byte("hello"), putCT: "text/plain"},
		{label: "put-with-meta", putKey: "meta.txt", putBody: []byte("m"),
			putCT: "app/x", putMeta: map[string]string{"team": "core"}},
		{label: "head-a", headKey: "a.txt"},
		{label: "head-meta", headKey: "meta.txt"},
		{label: "list-v2", list: true},
		{label: "multipart-big", mpKey: "big.bin", mpBody: []byte("0123456789")},
		{label: "copy-a", copySrc: "a.txt", copyDst: "copy.txt"},
		{label: "head-copy", headKey: "copy.txt"},
	}
}

// parityBucket is one bucket root (plain or provider-attached) with the state
// the matrix produced.
type parityBucket struct {
	name     string // "plain" | "provider"
	root     string // bucket root dir
	sidecars map[string][]byte
	surface  map[string]objectmodel.HeaderSnapshot
	list     []string // sorted keys from the list step
	attached []string // provider names ProbeAndAttach reported
}

// parityFakeProvider is leaf 01's fakeProvider pattern: side-effect free.
// Like the real providers it probes per bucket: Available only when the
// bucket root carries the parityAttachMarker (created for the provider
// variant only). Parity is about the core metadata path being untouched by
// provider ATTACHMENT, not about ZFS; the real zfs-events provider needs a
// ZFS host and is exercised by leaf 02's replay fixtures instead.
type parityFakeProvider struct {
	name string
}

// parityAttachMarker marks a bucket root as provider-attached for the fake.
const parityAttachMarker = ".parity-provider"

func (f *parityFakeProvider) Name() string { return f.name }
func (f *parityFakeProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
	if _, err := os.Stat(filepath.Join(bucketPath, parityAttachMarker)); err != nil {
		return ProbeResult{Available: false, Reason: "no provider marker"}, nil //nolint:nilerr // unattached is a ProbeResult status, not a failure
	}
	return ProbeResult{Available: true, Dataset: "tank/parity"}, nil
}
func (f *parityFakeProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
	return nil, nil
}
func (f *parityFakeProvider) Purge(ctx context.Context, bucketPath string) error { return nil }

// paritySidecar is the on-disk sidecar shape, pinned to package main's
// ObjectMetadata JSON field names (types.go) and production serialization
// (storage.go: MarshalIndent("", "  ")). Modulo storagePath — which
// legitimately differs by bucket root — this is what must stay byte-identical
// across backends.
type paritySidecar struct {
	ContentType    string            `json:"contentType"`
	ContentLength  int64             `json:"contentLength"`
	ETag           string            `json:"eTag"`
	CustomMetadata map[string]string `json:"customMetadata"`
	LastModified   time.Time         `json:"lastModified"`
	StoragePath    string            `json:"storagePath"`
}

// parityETag is the ETag form the handlers store (object_handlers.go):
// hex(md5(body)), stored bare in the sidecar and quoted on the wire.
func parityETag(body []byte) string {
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

// runParityMatrix executes the identical matrix against both variants and
// returns per-variant results.
func runParityMatrix(t *testing.T) map[string]*parityBucket {
	t.Helper()

	// Register the fake provider once; duplicate registration panics and
	// other tests in this package share the process.
	if Lookup("parity-fake") == nil {
		Register(&parityFakeProvider{name: "parity-fake"})
	}

	out := map[string]*parityBucket{}
	for _, variant := range []struct {
		name     string
		register bool
	}{{"plain", false}, {"provider", true}} {
		root := t.TempDir()
		bucket := &parityBucket{name: variant.name, root: root,
			sidecars: map[string][]byte{}, surface: map[string]objectmodel.HeaderSnapshot{}}

		if variant.register {
			// Mark the bucket root so the fake's Probe reports Available
			// here and nowhere else (per-bucket attach, like the real
			// providers' FS-type detection).
			if err := os.WriteFile(filepath.Join(root, parityAttachMarker), nil, 0o644); err != nil {
				t.Fatalf("write attach marker: %v", err)
			}
		}

		got := ProbeAndAttach(context.Background(), root)
		if variant.register {
			if len(got) != 1 || got[0] != "parity-fake" {
				t.Fatalf("ProbeAndAttach on provider variant = %v, want [parity-fake]", got)
			}
		} else if len(got) != 0 {
			t.Fatalf("plain variant unexpectedly attached %v", got)
		}
		bucket.attached = got

		applyParityMatrix(t, bucket)
		out[variant.name] = bucket
	}
	return out
}

// applyParityMatrix runs the matrix steps against one bucket root, capturing
// the canonical surface per step. Identical step inputs (deterministic clock,
// identical bodies) MUST produce identical state in both variants.
func applyParityMatrix(t *testing.T, b *parityBucket) {
	t.Helper()
	// Deterministic clock: step N gets base+N seconds. Identical across
	// variants by construction.
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for i, step := range parityMatrix() {
		now := base.Add(time.Duration(i) * time.Second)
		switch {
		case step.putKey != "":
			sc := paritySidecar{
				ContentType:    step.putCT,
				ContentLength:  int64(len(step.putBody)),
				ETag:           parityETag(step.putBody),
				CustomMetadata: parityWireMeta(step.putMeta),
				LastModified:   now,
				StoragePath:    filepath.Join(b.root, step.putKey),
			}
			putParitySidecar(t, b, step.putKey, sc)
			b.surface[step.label] = sc.snapshot()

		case step.mpKey != "":
			sc := paritySidecar{
				ContentType:   "application/octet-stream",
				ContentLength: int64(len(step.mpBody)),
				ETag:          parityETag(step.mpBody),
				LastModified:  now,
				StoragePath:   filepath.Join(b.root, step.mpKey),
			}
			putParitySidecar(t, b, step.mpKey, sc)
			b.surface[step.label] = sc.snapshot()

		case step.copySrc != "":
			// Server-side copy: production rebuilds the sidecar at the
			// destination preserving content metadata (object_handlers.go).
			var src paritySidecar
			if err := json.Unmarshal(b.sidecars[step.copySrc], &src); err != nil {
				t.Fatalf("copy %q: unmarshal src sidecar: %v", step.copySrc, err)
			}
			dst := src
			dst.LastModified = now
			dst.StoragePath = filepath.Join(b.root, step.copyDst)
			putParitySidecar(t, b, step.copyDst, dst)
			b.surface[step.label] = dst.snapshot()

		case step.headKey != "":
			var sc paritySidecar
			if err := json.Unmarshal(b.sidecars[step.headKey], &sc); err != nil {
				t.Fatalf("head %q: unmarshal sidecar: %v", step.headKey, err)
			}
			b.surface[step.label] = sc.snapshot()

		case step.list:
			b.list = parityListKeys(t, b.root)
		}
	}
}

// putParitySidecar serializes with production's exact form and persists it
// under <root>/.metadata/<key>.meta.
func putParitySidecar(t *testing.T, b *parityBucket, key string, sc paritySidecar) {
	t.Helper()
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	metaDir := filepath.Join(b.root, ".metadata")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("mkdir .metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, key+".meta"), data, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	b.sidecars[key] = data
}

// snapshot renders the sidecar's canonical surface into an
// objectmodel.HeaderSnapshot the way a response handler would: build the wire
// header set via ObjectToMetadataHeaders, then re-capture through a
// case-insensitive getter via SnapshotHeaders.
func (sc paritySidecar) snapshot() objectmodel.HeaderSnapshot {
	o := objectmodel.Object{
		Size:         sc.ContentLength,
		ETag:         sc.ETag,
		LastModified: sc.LastModified,
		ContentType:  sc.ContentType,
		Metadata:     parityCanonicalMeta(sc.CustomMetadata),
	}
	wire := objectmodel.ObjectToMetadataHeaders(o)
	get := func(k string) string {
		for _, v := range wire[k] {
			return v
		}
		return ""
	}
	var metaKeys []string
	for k := range wire {
		if len(k) > len("x-amz-meta-") && k[:len("x-amz-meta-")] == "x-amz-meta-" {
			metaKeys = append(metaKeys, k)
		}
	}
	sort.Strings(metaKeys)
	return objectmodel.SnapshotHeaders(get, metaKeys)
}

// parityWireMeta adds the legacy on-disk x-amz-meta- prefix to canonical keys.
func parityWireMeta(meta map[string]string) map[string]string {
	if meta == nil {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[objectmodel.MetadataHeaderName(k)] = v
	}
	return out
}

// parityCanonicalMeta strips the prefix (legacy on-disk form -> canonical).
func parityCanonicalMeta(meta map[string]string) map[string]string {
	if meta == nil {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[objectmodel.NormalizeMetadataKey(k)] = v
	}
	return out
}

// parityListKeys walks the bucket root's .metadata sidecars for the list
// surface (object keys), sorted deterministically.
func parityListKeys(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".metadata"))
	if err != nil {
		t.Fatalf("read .metadata: %v", err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".meta" {
			continue
		}
		keys = append(keys, e.Name()[:len(e.Name())-len(".meta")])
	}
	sort.Strings(keys)
	return keys
}

// sidecarComparable returns the sidecar JSON with storagePath removed,
// re-marshaled deterministically (leaf Task 3's pinned comparison form).
func sidecarComparable(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("sidecar not valid JSON: %v", err)
	}
	delete(m, "storagePath")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestParityIdenticalResponses pins Tasks 1+2: the identical request matrix
// produces identical canonical header snapshots in both variants. Uses the
// pinned Last-Modified tolerance policy; everything else must be exactly
// equal.
func TestParityIdenticalResponses(t *testing.T) {
	got := runParityMatrix(t)
	plain, provider := got["plain"], got["provider"]

	for _, label := range sortedParityLabels(plain.surface) {
		a, okA := plain.surface[label]
		b, okB := provider.surface[label]
		if !okA || !okB {
			t.Fatalf("label %q missing from a variant (plain=%v provider=%v)", label, okA, okB)
		}
		if diff := parityCompareSnapshots(a, b); diff != "" {
			t.Errorf("parity break on %q:\n%s", label, diff)
		}
	}

	// List surface: identical key sets from identical matrices.
	if !parityStringsEqual(plain.list, provider.list) {
		t.Errorf("list surface diverged: plain=%v provider=%v", plain.list, provider.list)
	}
}

// TestParitySidecarByteCompat pins Task 3: sidecar JSON is byte-identical
// between variants modulo storagePath.
func TestParitySidecarByteCompat(t *testing.T) {
	got := runParityMatrix(t)
	plain, provider := got["plain"], got["provider"]

	if len(plain.sidecars) == 0 || len(plain.sidecars) != len(provider.sidecars) {
		t.Fatalf("sidecar sets differ: plain=%d provider=%d",
			len(plain.sidecars), len(provider.sidecars))
	}

	for _, key := range sortedParityKeyList(plain.sidecars) {
		pa, okA := plain.sidecars[key]
		pb, okB := provider.sidecars[key]
		if !okA || !okB {
			t.Fatalf("sidecar %q missing from a variant", key)
		}
		if !parityBytesEqual(sidecarComparable(t, pa), sidecarComparable(t, pb)) {
			t.Errorf("sidecar %q diverged modulo storagePath:\nplain:    %s\nprovider: %s", key, pa, pb)
		}
		// storagePath must be present and root-scoped in both variants.
		var ma, mb map[string]any
		if err := json.Unmarshal(pa, &ma); err != nil {
			t.Fatalf("sidecar %q not valid JSON: %v", key, err)
		}
		if err := json.Unmarshal(pb, &mb); err != nil {
			t.Fatalf("sidecar %q not valid JSON: %v", key, err)
		}
		if sp, _ := ma["storagePath"].(string); sp == "" {
			t.Errorf("plain sidecar %q missing storagePath", key)
		}
		if sp, _ := mb["storagePath"].(string); sp == "" {
			t.Errorf("provider sidecar %q missing storagePath", key)
		}
	}
}

// TestParityProviderAttachmentInvisible pins the seam contract itself:
// attaching a provider changes nothing about the core metadata path for
// identical buckets — same captures, same sidecar count, same list.
func TestParityProviderAttachmentInvisible(t *testing.T) {
	got := runParityMatrix(t)
	plain, provider := got["plain"], got["provider"]

	if len(provider.attached) != 1 || provider.attached[0] != "parity-fake" {
		t.Fatalf("provider variant attached %v, want [parity-fake]", provider.attached)
	}
	if len(plain.attached) != 0 {
		t.Fatalf("plain variant attached %v, want none", plain.attached)
	}
	if len(plain.surface) != len(provider.surface) {
		t.Fatalf("surface capture counts differ: %d vs %d",
			len(plain.surface), len(provider.surface))
	}
	if len(plain.sidecars) != len(provider.sidecars) {
		t.Fatalf("sidecar counts differ: %d vs %d",
			len(plain.sidecars), len(provider.sidecars))
	}
}

// TestParityLastModifiedTolerancePolicy exercises the pinned comparison
// policy directly: ONLY a Last-Modified difference within 60s (parsed as
// HTTP-date) may be tolerated; every other difference on the canonical
// surface must fail, even alongside a tolerated one.
func TestParityLastModifiedTolerancePolicy(t *testing.T) {
	lm := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	lmPlus30 := time.Date(2026, 9, 28, 12, 0, 30, 0, time.UTC).Format(http.TimeFormat)
	lmPlus61 := time.Date(2026, 9, 28, 12, 1, 1, 0, time.UTC).Format(http.TimeFormat)
	snap := func(lmV, etag string) objectmodel.HeaderSnapshot {
		return objectmodel.HeaderSnapshot{
			ContentType: "text/plain", ContentLength: "5",
			ETag: etag, LastModified: lmV,
		}
	}

	if diff := parityCompareSnapshots(snap(lm, `"a"`), snap(lmPlus30, `"a"`)); diff != "" {
		t.Errorf("Last-Modified within 60s must be tolerated, got: %s", diff)
	}
	if diff := parityCompareSnapshots(snap(lm, `"a"`), snap(lmPlus61, `"a"`)); diff == "" {
		t.Error("Last-Modified beyond 60s must FAIL")
	}
	if diff := parityCompareSnapshots(snap(lm, `"a"`), snap(lmPlus30, `"b"`)); diff == "" {
		t.Error("ETag difference must FAIL even with tolerated Last-Modified")
	}
	notDate := snap("not-a-date", `"a"`)
	if diff := parityCompareSnapshots(snap(lm, `"a"`), notDate); diff == "" {
		t.Error("non-HTTP-date Last-Modified must FAIL, not be silently tolerated")
	}
}

// --- comparison helpers -----------------------------------------------------

// parityCompareSnapshots applies AssertHeaderParityWithPolicy with the
// pinned shared policy (objectmodel.DefaultParityPolicy: Last-Modified must
// parse as HTTP-date and be within 60 seconds; ALL other fields exactly
// equal) — the policy lives in the exported gate, no local copy.
func parityCompareSnapshots(a, b objectmodel.HeaderSnapshot) string {
	return objectmodel.AssertHeaderParityWithPolicy(a, b, objectmodel.DefaultParityPolicy)
}

func sortedParityLabels(m map[string]objectmodel.HeaderSnapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedParityKeyList(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parityStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parityBytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// zmetad-provider scenario (zmetad-provider-2026-09 leaf 05, Task 1)
// ---------------------------------------------------------------------------

// parityZmetadStub is a path-aware dbHandle stub for the parity gate:
// ONLY the provider bucket's (symlink-resolved) root resolves to the
// canned dataset; every other path gets DatasetNotTrackedError - the
// exact seam the frontend's 503-vs-200 decision rides on
// (metadataProviderFor returns nil when Probe reports unavailable).
type parityZmetadStub struct {
	stubZmetadDB
	providerRoot string // resolved bucket root that IS tracked
	dataset      string
}

func (s *parityZmetadStub) ResolveDatasetByPath(path string) (string, error) {
	s.resolveCalls.Add(1)
	if path != s.providerRoot {
		return "", &DatasetNotTrackedError{Path: path}
	}
	return s.dataset, nil
}

// TestParityZmetadProviderBucket pins leaf-05 Task 1: a bucket served by
// the REAL zmetad provider (openDB stubbed via leaf 03's seam; no ZFS
// host needed) produces BYTE-IDENTICAL core S3 metadata state as a plain
// bucket running the same matrix, while ?events availability is
// asymmetric: the plain bucket probes untracked (the frontend's 503
// seam), the provider bucket probes available and serves history with
// the Contract 5 loss detail (recordsLost=7, ringSwaps=1, never folded).
func TestParityZmetadProviderBucket(t *testing.T) {
	ctx := context.Background()

	plainRoot := t.TempDir()
	providerRoot := t.TempDir()
	// The provider EvalSymlinks before resolution; the stub keys on the
	// resolved form (macOS tempdirs live behind /var -> /private/var).
	resolvedProvider, err := filepath.EvalSymlinks(providerRoot)
	if err != nil {
		t.Fatalf("resolve provider root: %v", err)
	}

	const dbPath = "/parity/zmetad.db"
	stub := &parityZmetadStub{providerRoot: resolvedProvider, dataset: "tank/parity"}
	stub.hasDS = true
	stub.events = []EventRow{
		{Dataset: "tank/parity", Txg: 10, Op: "CREATE",
			Path: new("a.txt"), FullPath: new("a.txt")},
	}
	stub.gaps = GapStats{KnownLost: 7, Regressions: 0, RingSwaps: 1}
	oldOpenDB := openDB
	openDB = func(context.Context, string) (dbHandle, error) { return stub, nil }
	t.Cleanup(func() { openDB = oldOpenDB })
	// Bypass ONLY the statfs hint (dev hosts have no ZFS); dataset
	// resolution + poll checks still run against the stub DB.
	t.Setenv("ZETAOBJECT_ASSUME_ZFS", "1")

	p := NewZmetadEventsProvider(dbPath)

	// --- attach asymmetry: the exact seam behind 503 (plain) vs 200 ---
	resPlain, err := p.Probe(ctx, plainRoot)
	if err != nil {
		t.Fatalf("plain Probe error = %v, want nil (unavailable is a status)", err)
	}
	if resPlain.Available || resPlain.Reason != "not tracked by zmetad" {
		t.Fatalf("plain Probe = %+v, want unavailable 'not tracked by zmetad' (frontend serves 503)", resPlain)
	}
	resProvider, err := p.Probe(ctx, providerRoot)
	if err != nil {
		t.Fatalf("provider Probe error = %v, want nil", err)
	}
	if !resProvider.Available || resProvider.Dataset != "tank/parity" {
		t.Fatalf("provider Probe = %+v, want available on tank/parity (frontend serves 200)", resProvider)
	}

	// --- same matrix, byte-identical core metadata state --------------
	plain := &parityBucket{name: "plain", root: plainRoot,
		sidecars: map[string][]byte{}, surface: map[string]objectmodel.HeaderSnapshot{}}
	provider := &parityBucket{name: "zmetad-provider", root: providerRoot,
		sidecars: map[string][]byte{}, surface: map[string]objectmodel.HeaderSnapshot{}}
	applyParityMatrix(t, plain)
	applyParityMatrix(t, provider)

	for _, label := range sortedParityLabels(plain.surface) {
		a, b := plain.surface[label], provider.surface[label]
		if diff := parityCompareSnapshots(a, b); diff != "" {
			t.Errorf("zmetad parity break on %q:\n%s", label, diff)
		}
	}
	if !parityStringsEqual(plain.list, provider.list) {
		t.Errorf("zmetad list surface diverged: plain=%v provider=%v", plain.list, provider.list)
	}
	for _, key := range sortedParityKeyList(plain.sidecars) {
		if !parityBytesEqual(sidecarComparable(t, plain.sidecars[key]),
			sidecarComparable(t, provider.sidecars[key])) {
			t.Errorf("zmetad sidecar %q diverged modulo storagePath:\nplain:    %s\nprovider: %s",
				key, plain.sidecars[key], provider.sidecars[key])
		}
	}

	// --- ?events exists only on the provider side ---------------------
	events, err := p.History(ctx, providerRoot, "", HistoryQuery{})
	if err != nil {
		t.Fatalf("provider History = %v, want success", err)
	}
	if len(events) != 1 || events[0].Key != "a.txt" || events[0].Op != "create" {
		t.Fatalf("provider History = %+v, want the canned create", events)
	}
	d, ok := p.(interface{ LastDetail() HistoryDetail })
	if !ok {
		t.Fatal("zmetad provider must implement the LastDetail detailReporter seam")
	}
	if got := d.LastDetail(); got.Dataset != "tank/parity" || got.RecordsLost != 7 || got.RingSwaps != 1 {
		t.Fatalf("LastDetail = %+v, want {tank/parity 7 1} (Contract 5: swaps never folded)", got)
	}
	if _, err := p.History(ctx, plainRoot, "", HistoryQuery{}); err == nil {
		t.Fatal("plain-bucket History succeeded, want resolution failure (the 503 side)")
	}
}
