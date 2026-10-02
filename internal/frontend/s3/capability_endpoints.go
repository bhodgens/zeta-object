package s3

// capability_endpoints.go — metadata-zfs leaf 04: the MetadataProvider
// capability endpoints exposed through the existing S3 subresource
// dispatch (exactly like ?acl / ?uploads / ?uploadId):
//
//	GET /{bucket}/{key}?events          → JSON ObjectEventHistory (key-scoped)
// GET /{bucket}?events                → JSON ObjectEventHistory (bucket summary)
// GET /{bucket}?events&versions       → XML ListObjectVersionsExt (DERIVED,
//                                       NON-STANDARD, LOSSY — not S3 versioning;
//                                       prefix is honored; delimiter and
//                                       encoding-type are NOT — documented limitation)
//
// Auth is inherited from the serveHTTP pipeline (SigV4 runs before any
// dispatch); these handlers add no auth code of their own. Errors go
// through writeS3Error; provider/exec error detail is NEVER sent to the
// client (logged server-side only).
//
// HONESTY CONTRACTS (pinned in metadata-zfs master Contract 4):
//   - Event timestamps are best-effort only: the upstream wire format
//     carries no wall-clock time, so JSON `timestamp` serializes as the
//     zero time and XML LastModified is empty. NEVER fabricated, and
//     responses are ordered by ring-buffer stream order (newest first),
//     never by timestamp.
//   - VersionIds ("txg-<txg>-<index>", index = position in THIS
//     response's event stream) are stable only within one response —
//     ring buffers drop records, so indices shift across calls. This is
//     exactly why the response carries IsLossy/RecordsLost: clients
//     must not treat these ids as durable S3 version handles.
//   - Derivation rules for ?events&versions: create/rename/truncate →
//     version entry (Size = SizeNew); remove → IsDeleteMarker entry;
//     link/symlink/setattr → no entry (v1). IsLatest = first entry per
//     key in stream order. IsLossy=true + RecordsLost>0 whenever the
//     provider reports ring-buffer loss.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/metadata"
)

// zfsEventsProviderName is the reserved provider name these endpoints
// resolve (leaf 01's registry name).
const zfsEventsProviderName = "zfs-events"

// metadataProviderHook is the per-bucket provider-resolution seam. The
// wiring layer (or tests) may install a resolver keyed by bucket path —
// the durable design is a startup-built attach map; nil falls back to
// the probe-on-request shim below, which backend-interface replaces by
// swapping this one function's callers. Guarded by hookMu (seam.go).
var metadataProviderHook func(bucketPath string) metadata.MetadataProvider

// InstallMetadataProvider installs the per-bucket provider resolver
// (exported wiring entry; nil restores the probe-on-request shim).
func InstallMetadataProvider(fn func(bucketPath string) metadata.MetadataProvider) {
	hookMu.Lock()
	defer hookMu.Unlock()
	metadataProviderHook = fn
}

// metadataProviderFor resolves the attached MetadataProvider for a
// bucket path, or nil when none is attached. The default shim probes
// the registered zfs-events provider against the bucket path —
// per-bucket attach semantics, same as the startup ProbeAndAttach flow.
func metadataProviderFor(bucketPath string) metadata.MetadataProvider {
	hookMu.RLock()
	fn := metadataProviderHook
	hookMu.RUnlock()
	if fn != nil {
		return fn(bucketPath)
	}
	p := metadata.Lookup(zfsEventsProviderName)
	if p == nil {
		return nil
	}
	res, err := p.Probe(context.Background(), bucketPath)
	if err != nil || !res.Available {
		return nil
	}
	return p
}

// detailReporter is the optional seam for providers that carry their own
// HistoryDetail (the frozen MetadataProvider interface MUST NOT gain
// methods, so detail is read structurally).
type detailReporter interface {
	LastDetail() metadata.HistoryDetail
}

// principalReporter is the optional seam for backends that record
// principal breadcrumbs (auth extensions leaf 10): Owner reports the
// object's xattr owner when one exists. The frozen MetadataProvider
// interface MUST NOT gain methods — the enrichment is type-asserted,
// exactly like detailReporter above.
type principalReporter interface {
	Owner(bucket, key string) (string, bool)
}

// principalReporterFor type-asserts the bucket's Backend.
func principalReporterFor(bucket string) principalReporter {
	if b, err := backendFor(bucket); err == nil && b != nil {
		if pr, ok := b.(principalReporter); ok {
			return pr
		}
	}
	return nil
}

// historyDetailFor reads the detail of the most recent completed History
// call: the provider's own hook when it implements one. Providers that do
// NOT implement LastDetail get a zero HistoryDetail (no dataset, no loss
// claims) - the package-global fallback died with the CLI transport
// (zmetad-provider-2026-09 leaf 04): a global last-detail record
// cross-attributes between concurrent ?events on different buckets
// (bughunt A2/C2), so the structural per-instance interface is the ONLY
// path.
func historyDetailFor(p metadata.MetadataProvider) metadata.HistoryDetail {
	if d, ok := p.(detailReporter); ok {
		return d.LastDetail()
	}
	return metadata.HistoryDetail{}
}

// ObjectEventHistory is the JSON envelope for ?events responses.
// RingSwaps counts kernel-log identity swaps (zmetad gaps rows with the
// lost=-1 sentinel) - a SEPARATE loss class from recordsLost, never
// folded into it (zmetad SCHEMA.md section 4, master Contract 5).
type ObjectEventHistory struct {
	Dataset     string            `json:"dataset"`
	RecordsLost uint64            `json:"recordsLost"`
	RingSwaps   uint64            `json:"ringSwaps"`
	Events      []objectEventJSON `json:"events"`
}

// objectEventJSON is the wire form of one event. Op is lowercased;
// timestamp is RFC3339 best-effort (zero time serializes as
// "0001-01-01T00:00:00Z" — never replaced with a fabricated value).
// C8 (bughunt-gateway-2026-09-29): uid/gid are intentionally NOT part of
// the wire form — client-visible owner identity is unnecessary surface.
// Owner (auth extensions leaf 10) is the object's xattr breadcrumb
// (user.zeta.owner), present ONLY when the object carries one — never
// fabricated; unstamped objects keep the exact pre-change shape.
type objectEventJSON struct {
	Op        string `json:"op"`
	Key       string `json:"key,omitempty"`
	OldKey    string `json:"oldKey,omitempty"`
	Txg       uint64 `json:"txg"`
	Timestamp string `json:"timestamp"` // RFC3339; zero-time is honest "unknown"
	SizeOld   int64  `json:"sizeOld,omitempty"`
	SizeNew   int64  `json:"sizeNew,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

// ListObjectVersionsExt is the ?events&versions XML document — a
// DERIVED, LOSSY, NON-STANDARD extension (never real S3 versioning).
// IsLossy = RecordsLost > 0 OR RingSwaps > 0 (Contract 5: swaps are a
// separate loss class and never folded into RecordsLost).
type ListObjectVersionsExt struct {
	XMLName     xml.Name           `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListObjectVersionsExt"`
	Name        string             `xml:"Name"`
	IsLossy     bool               `xml:"IsLossy"`
	RecordsLost uint64             `xml:"RecordsLost"`
	RingSwaps   uint64             `xml:"RingSwaps"`
	Version     []ObjectVersionExt `xml:"Version"`
}

// ObjectVersionExt is one derived version entry (or delete marker).
type ObjectVersionExt struct {
	Key            string `xml:"Key"`
	VersionId      string `xml:"VersionId"` // "txg-<txg>-<index>"; response-scoped, not durable
	IsLatest       bool   `xml:"IsLatest"`
	LastModified   string `xml:"LastModified"` // empty: upstream wire has no time field yet
	Size           int64  `xml:"Size"`
	Op             string `xml:"Op"` // originating event op: create|truncate|rename|remove
	IsDeleteMarker bool   `xml:"IsDeleteMarker"`
}

// handleObjectEvents serves GET /{bucket}/{key}?events (JSON).
func handleObjectEvents(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	if !resolveEventsContext(w, bucketName) {
		return
	}
	p := metadataProviderFor(getBucketPath(bucketName))
	if p == nil {
		writeNoProviderError(w)
		return
	}
	q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
	events, err := p.History(r.Context(), getBucketPath(bucketName), objectName, q)
	if err != nil {
		// NO provider/exec detail reaches the client — server log only.
		log.Printf("metadata: events for %s/%s: %v", bucketName, objectName, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
		return
	}
	detail := historyDetailFor(p)
	writeEventsJSON(w, ObjectEventHistory{
		Dataset:     detail.Dataset,
		RecordsLost: detail.RecordsLost,
		RingSwaps:   detail.RingSwaps,
		Events:      toEventJSON(events, eventsOwnerLookup(bucketName)),
	})
}

// handleBucketEvents serves GET /{bucket}?events and ?events&versions
// (JSON summary; XML version listing when "versions" is present).
func handleBucketEvents(w http.ResponseWriter, r *http.Request, bucketName string) {
	if !resolveEventsContext(w, bucketName) {
		return
	}
	bucketPath := getBucketPath(bucketName)
	p := metadataProviderFor(bucketPath)
	if p == nil {
		writeNoProviderError(w)
		return
	}
	q := metadata.HistoryQuery{MaxEvents: maxEventsFromQuery(r)}
	events, err := p.History(r.Context(), bucketPath, "", q)
	if err != nil {
		log.Printf("metadata: events for %s: %v", bucketName, err) //nolint:gosec // G706: strconvQuote-sanitized / constant-only format
		writeS3Error(w, "InternalError", "unable to read event history", http.StatusInternalServerError)
		return
	}
	detail := historyDetailFor(p)

	// ?versions sub-sub-resource: the derived, lossy XML extension.
	if _, ok := r.URL.Query()["versions"]; ok {
		ext := versionsFromEvents(events, detail, r.URL.Query().Get("prefix"))
		ext.Name = bucketName // S3 Name convention: the bucket name
		writeXML(w, http.StatusOK, ext)
		return
	}
	writeEventsJSON(w, ObjectEventHistory{
		Dataset:     detail.Dataset,
		RecordsLost: detail.RecordsLost,
		RingSwaps:   detail.RingSwaps,
		Events:      toEventJSON(events, eventsOwnerLookup(bucketName)),
	})
}

// eventsOwnerLookup returns the per-event owner resolution func for a
// bucket: the type-asserted principalReporter when the backend records
// breadcrumbs, else always-absent (plain buckets return the identical
// pre-change shape — the parity gate). A nil reporter never fabricates.
func eventsOwnerLookup(bucketName string) func(key string) (string, bool) {
	pr := principalReporterFor(bucketName)
	if pr == nil {
		return func(string) (string, bool) { return "", false }
	}
	return func(key string) (string, bool) { return pr.Owner(bucketName, key) }
}

// resolveEventsContext validates the bucket for an ?events request:
// names that fail validBucket (S3 naming rules, or traversal like
// /..%2f..%2fetc which would otherwise escape dataDir and pass the
// exists check) and unknown buckets keep the standard 404 (404
// precedence over provider resolution). Returns false when it has
// written the error.
func resolveEventsContext(w http.ResponseWriter, bucketName string) bool {
	if !validBucket(bucketName) || !bucketExists(bucketName) {
		log.Printf("Bucket %s does not exist for ?events", strconv.Quote(bucketName))
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
		return false
	}
	return true
}

// writeNoProviderError writes the contracted 503 for a bucket with no
// attached metadata provider.
func writeNoProviderError(w http.ResponseWriter) {
	writeS3Error(w, "NotImplemented", "no metadata provider available for this bucket", http.StatusServiceUnavailable)
}

// writeEventsJSON writes the ?events JSON envelope (compact, matching
// the repo's JSON conventions elsewhere in the wire path).
func writeEventsJSON(w http.ResponseWriter, v ObjectEventHistory) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("Error marshalling %T to JSON: %v", v, err)
		writeS3Error(w, "InternalError", "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// toEventJSON maps provider events to the wire form. Op is lowercased
// defensively (the zfs parser already lowercases); timestamps pass
// through honestly (zero stays zero). ownerOf resolves the object's xattr
// owner per event; absent owners leave the field out entirely (omitempty
// — never fabricated).
func toEventJSON(events []metadata.ObjectEvent, ownerOf func(key string) (string, bool)) []objectEventJSON {
	out := make([]objectEventJSON, 0, len(events))
	for _, e := range events {
		ev := objectEventJSON{
			Op:        strings.ToLower(e.Op),
			Key:       e.Key,
			OldKey:    e.OldKey,
			Txg:       e.Txg,
			Timestamp: e.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
			SizeOld:   e.SizeOld,
			SizeNew:   e.SizeNew,
		}
		if ownerOf != nil {
			if owner, ok := ownerOf(e.Key); ok {
				ev.Owner = owner
			}
		}
		out = append(out, ev)
	}
	return out
}

// versionsFromEvents derives the version listing from the event stream.
//
// PINNED derivation rules (metadata-zfs master decision 4):
//   - events arrive newest-first (provider ring-buffer stream order);
//     that order — never timestamps — defines recency here.
//   - prefix: events whose reconstructed key lacks the prefix produce
//     no version entry (D4, bughunt-postF2-2026-09-29).
//   - delimiter and encoding-type are NOT honored (documented limitation).
//   - create / rename / truncate → version entry: Size = SizeNew,
//     VersionId = "txg-<txg>-<index>" with index = position in THIS
//     event slice (txg alone is not unique; the id is deliberately
//     response-scoped because ring buffers make it non-durable).
//   - remove → IsDeleteMarker entry.
//   - link / symlink / setattr → no entry (v1).
//   - IsLatest = first entry per key in stream order.
//   - IsLossy/RecordsLost surface ring-buffer loss so clients know the
//     listing (and its ids) is incomplete.
func versionsFromEvents(events []metadata.ObjectEvent, detail metadata.HistoryDetail, prefix string) ListObjectVersionsExt {
	out := ListObjectVersionsExt{
		Name:        detail.Dataset,
		IsLossy:     detail.RecordsLost > 0 || detail.RingSwaps > 0,
		RecordsLost: detail.RecordsLost,
		RingSwaps:   detail.RingSwaps,
	}
	latest := map[string]bool{}
	for i, e := range events { // newest-first (stream order)
		if prefix != "" && !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		v := ObjectVersionExt{Key: e.Key}
		switch strings.ToLower(e.Op) {
		case "create", "rename", "truncate":
			v.VersionId = fmt.Sprintf("txg-%d-%d", e.Txg, i)
			v.Size = e.SizeNew
			v.Op = strings.ToLower(e.Op)
		case "remove":
			v.VersionId = fmt.Sprintf("txg-%d-%d", e.Txg, i)
			v.Op = "remove"
			v.IsDeleteMarker = true
		default:
			continue // link/symlink/setattr: no version entry in v1
		}
		v.IsLatest = !latest[e.Key]
		latest[e.Key] = true
		out.Version = append(out.Version, v)
	}
	return out
}

// eventsMaxEventsDefault / eventsMaxEventsLimit bound the max-events
// query parameter (default 1000, hard cap 10000).
const (
	eventsMaxEventsDefault = 1000
	eventsMaxEventsLimit   = 10000
)

// maxEventsFromQuery parses the ?max-events cap: absent/invalid → the
// default; bounded to [1, 10000].
func maxEventsFromQuery(r *http.Request) int {
	s := r.URL.Query().Get("max-events")
	if s == "" {
		return eventsMaxEventsDefault
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return eventsMaxEventsDefault
	}
	if n > eventsMaxEventsLimit {
		return eventsMaxEventsLimit
	}
	return n
}
