package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// zfsRunner is the seam tests replace. Production wiring execs the zfs CLI:
// argv-only (never a shell string — dataset names come from `zfs get`
// output, never user input), context timeout, stderr captured.
var zfsRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zfs", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

func runZFS(ctx context.Context, args ...string) ([]byte, string, error) {
	return zfsRunner(ctx, args...)
}

// resolveDatasetFn lets tests stub dataset resolution (leaf 01's
// ResolveDataset shells out to the real zfs binary via its own path, which
// the zfsRunner seam does not cover).
var resolveDatasetFn = ResolveDataset

// HistoryDetail carries information ObjectEvent cannot: lossiness of the
// ring buffer and the source dataset.
//
// DEPRECATED for production use — TEST-ONLY (bughunt A2/C2): this
// package-global stores the most recent completed History call, so two
// concurrent ?events requests on different buckets can read each other's
// dataset/recordsLost. The production path is the provider-instance
// detail seam: zfsEventsProvider carries its own HistoryDetail (read via
// the frontend's detailReporter interface, capability_endpoints.go
// historyDetailFor), and this global is now only consulted by tests and
// by providers that do not implement LastDetail().
//
// Detail is served through a package-level hook rather than a method on
// the frozen MetadataProvider interface: the interface MUST NOT gain
// methods (master Contract 1).
func LastHistoryDetail() HistoryDetail {
	detailMu.Lock()
	defer detailMu.Unlock()
	return lastDetail
}

type HistoryDetail struct {
	Dataset     string
	RecordsLost uint64 // nonzero = history is lossy; surfaces records_lost
}

var (
	detailMu   sync.Mutex
	lastDetail HistoryDetail
)

func setHistoryDetail(d HistoryDetail) {
	detailMu.Lock()
	defer detailMu.Unlock()
	lastDetail = d
}

// rawEvent mirrors the pinned print_event JSON (zfs-metadata
// cmd/zfs/zfs_main.c:8332): all fields optional except txg/object/op, plus
// forward-compatible kernel fields the CLI may emit later (time, uid, gid,
// mode, attrs per include/sys/zfs_events.h). Pointer types distinguish
// "absent" from "present and zero" so we never fabricate metadata.
type rawEvent struct {
	Txg     uint64  `json:"txg"`
	Object  uint64  `json:"object"`
	Op      string  `json:"op"`
	Name    *string `json:"name"`
	Parent  uint64  `json:"parent"`
	OldName *string `json:"old_name"`
	// OldParent: parent dir of the pre-rename name (0 omitted on the wire).
	OldParent uint64  `json:"old_parent"`
	Target    *string `json:"target"`
	OldSize   int64   `json:"old_size"`
	NewSize   int64   `json:"new_size"`
	// TimeNs is the forward-compatible kernel `time` field: gethrtime()
	// output (zfs_events.c) — high-resolution, boot-relative on some
	// platforms, NOT guaranteed wall-clock. Treated as ordering
	// information; a future upstream change to wall-clock hrtime is an
	// upstream contract change caught by the replay fixture test.
	TimeNs uint64 `json:"time"`
	UID    uint64 `json:"uid"`
	GID    uint64 `json:"gid"`
	// Mode and Attrs are accepted for forward compatibility (kernel emits
	// them under the leaf-brief's extended print_event) but have no
	// ObjectEvent mapping yet.
	Mode  uint64 `json:"mode"`
	Attrs uint64 `json:"attrs"`
}

// recordsLostRe matches the plaintext trailing line print_event emits after
// the JSON array when the ring buffer dropped records.
//
// Wording limitation (pinned): upstream print_event emits exactly one
// variant, "<N> record(s) lost to log wraparound" (zfs-metadata
// cmd/zfs/zfs_main.c), so only the "record(s)" form is matched here. If a
// future upstream changes the wording (e.g. singular "1 record was lost"),
// this regex will miss it and the trailer parse will fall into the
// conservative unknown-loss path below — a comment-and-test update, not a
// data-loss bug.
var recordsLostRe = regexp.MustCompile(`(?m)^(\d+) record\(s\) lost`)

// parseEventsOutput parses `zfs events -j` stdout: a JSON array optionally
// followed by a plaintext "N record(s) lost to log wraparound" line.
// Unknown wire ops are lowercased and preserved verbatim (never an error);
// absent optional fields leave ObjectEvent zero values — never fabricated.
//
// Keys are reconstructed to full S3 paths from the object-id graph before
// returning (F-live-1): the log only carries bare names + parent object
// ids, so History's key filter needs the resolved paths. See
// reconstructPaths.
func parseEventsOutput(out string) ([]ObjectEvent, uint64, error) {
	set, lost, err := parseEventSet(out)
	if err != nil {
		return nil, 0, err
	}
	return set.events, lost, nil
}

// parseEventSet is parseEventsOutput with per-event path-resolution state
// attached, which History's key filter needs to tell fully-resolved keys
// (exact match) from partial ones (conservative broad match).
func parseEventSet(out string) (*eventSet, uint64, error) {
	end := strings.LastIndex(out, "]")
	if end < 0 {
		return nil, 0, fmt.Errorf("metadata: zfs events output has no JSON array: %q", truncateForErr(out))
	}
	var raws []rawEvent
	if err := json.Unmarshal([]byte(out[:end+1]), &raws); err != nil {
		return nil, 0, fmt.Errorf("metadata: parsing zfs events JSON: %w", err)
	}
	var lost uint64
	if m := recordsLostRe.FindStringSubmatch(out); m != nil {
		parsed, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			// Loss UNKNOWN, not zero: the trailer is present but its
			// count does not fit uint64 (corrupt/absurd output). Zero
			// would silently claim lossless history — conservative
			// sentinel 1 keeps the lossy signal surfaced to the
			// records_lost field instead.
			lost = 1
		} else if parsed == 0 {
			lost = 1 // same sentinel for a literal "0 record(s) lost" trailer
		} else {
			lost = parsed
		}
	}
	return reconstructPaths(raws), lost, nil
}

// maxPathDepth bounds parent-chain walks: object ids can repeat across a
// wraparound-compacted log, and a cycle in the reconstructed graph must
// terminate (partial result), not hang the request goroutine. Real ZFS
// datasets are far shallower.
const maxPathDepth = 64

// objEntry is one object-id -> (bare name, parent dir object id) mapping
// harvested from the event stream.
type objEntry struct {
	name   string
	parent uint64
}

// eventSet is the parsed event stream plus per-event path-resolution
// state. resolved[i] is true when events[i].Key was reconstructed to a
// full S3 key through the object-id graph; oldResolved[i] is the same for
// OldKey. Partial events keep their bare ZFS name and match keys
// conservatively (see eventMatchesKey).
type eventSet struct {
	events      []ObjectEvent
	resolved    []bool
	oldResolved []bool
}

// reconstructPaths builds the objid->name graph from the raw records and
// resolves every event's Key/OldKey to a full S3 path where the parent
// chain is complete (F-live-1: `zfs events -j` emits bare names only —
// verified against print_event, zfs_main.c:8332, which never emits paths).
//
// Graph rules pinned from the live zfs-meta host (OpenZFS 2.4.1
// extended-metadata branch):
//   - every record carries "object"; CREATE/LINK/RENAME/REMOVE records
//     carry "name" and (when nonzero) "parent" = containing dir object id;
//   - RENAME: name = new bare name, old_name = old bare name, parent =
//     old_parent = the (unchanged) parent dir object id;
//   - directory CREATEs appear too, with their own object id — that is
//     the graph walked here.
func reconstructPaths(raws []rawEvent) *eventSet {
	set := &eventSet{
		events:      make([]ObjectEvent, 0, len(raws)),
		resolved:    make([]bool, len(raws)),
		oldResolved: make([]bool, len(raws)),
	}
	byID := make(map[uint64]objEntry, len(raws))
	// Newest mapping wins: ring buffers return newest-first, so the FIRST
	// record seen for an object id is the newest. Object ids are reused
	// after wraparound compaction; stale entries are never overwritten.
	// (The oldest-first replay fixtures only contain distinct ids, so
	// order does not change their result.)
	for _, r := range raws {
		if r.Name == nil || r.Object == 0 {
			continue
		}
		if _, dup := byID[r.Object]; !dup {
			byID[r.Object] = objEntry{name: *r.Name, parent: r.Parent}
		}
	}
	root, haveRoot := detectRoot(raws, byID)
	for i, r := range raws {
		op := strings.ToLower(r.Op)
		e := ObjectEvent{Op: op, Txg: r.Txg}
		if r.Name != nil {
			e.Key = *r.Name
			parent := r.Parent
			if parent == 0 && op == "rename" {
				// print_event emits `parent` alongside `old_parent`
				// for RENAME (same dir), but tolerate its absence.
				parent = r.OldParent
			}
			if full, ok := resolvePath(byID, root, haveRoot, *r.Name, parent); ok {
				e.Key = full
				set.resolved[i] = true
			}
		}
		if r.OldName != nil {
			e.OldKey = *r.OldName
			parent := r.OldParent
			if parent == 0 {
				parent = r.Parent
			}
			if full, ok := resolvePath(byID, root, haveRoot, *r.OldName, parent); ok {
				e.OldKey = full
				set.oldResolved[i] = true
			}
		}
		// Sizes are only meaningful for truncate on the wire (print_event
		// emits them unconditionally only in that branch).
		if op == "truncate" {
			e.SizeOld, e.SizeNew = r.OldSize, r.NewSize
		}
		if r.TimeNs != 0 {
			e.Timestamp = time.Unix(0, int64(r.TimeNs)) //nolint:gosec // G115: zfs events -j emits ns offsets that fit int64 for real timestamps
		}
		e.UID, e.GID = uint32(r.UID), uint32(r.GID) //nolint:gosec // G115: uid/gid are 32-bit on every platform zfs events reports; truncation matches zfs behavior
		set.events = append(set.events, e)
	}
	return set
}

// detectRoot identifies the dataset root directory's object id so
// parent-chain walks can terminate. The root predates the ring buffer, so
// its id never appears as an "object" — only as a "parent" of top-level
// entries.
//
// A wrong root claim fabricates wrong EXACT keys (the F-live-1
// silent-empty failure in reverse), so detection is strict:
//
//  1. Only KNOWN directories vote — object ids that appear both as a
//     created object and as some record's parent. A lost directory's id
//     keeps appearing as an unresolvable parent with high frequency, but
//     it is provably not the root of any known dir above it.
//  2. Each known dir climbs its own resolvable ancestor chain; the first
//     id ABOVE the chain (absent from the object map) is that dir's root
//     candidate — the root, or an ancestor lost to wraparound.
//  3. Among candidates the most frequent wins (the true root is
//     referenced by every surviving top-level dir), ties to the smaller
//     id for determinism.
//
// With no known directory in the window (entire window inside a dir
// whose create was lost) detection is refused: everything stays partial
// and matches conservatively.
func detectRoot(raws []rawEvent, byID map[uint64]objEntry) (uint64, bool) {
	isParent := make(map[uint64]bool)
	for _, r := range raws {
		if r.Name == nil {
			continue
		}
		if r.Parent != 0 {
			isParent[r.Parent] = true
		}
		if strings.ToLower(r.Op) == "rename" && r.OldParent != 0 {
			isParent[r.OldParent] = true
		}
	}
	freq := make(map[uint64]int)
	known := false
	for id := range isParent {
		ent, ok := byID[id]
		if !ok {
			continue // referenced dir whose create is outside the window
		}
		known = true
		// Climb this dir's resolvable ancestor chain; the id above it
		// is the root candidate. Depth-capped against cycles from
		// wraparound object-id reuse.
		for range maxPathDepth {
			next, ok := byID[ent.parent]
			if !ok {
				if ent.parent != 0 {
					freq[ent.parent]++
				}
				break
			}
			ent = next
		}
	}
	if !known {
		return 0, false
	}
	root, best := uint64(0), 0
	for id, n := range freq {
		if n > best || (n == best && id < root) {
			root, best = id, n
		}
	}
	if best == 0 {
		return 0, false
	}
	return root, true
}

// resolvePath reconstructs the full S3 key for a bare name under the
// directory object id parent by walking parent -> grandparent until the
// dataset root. ok=false means the chain could not be fully resolved —
// records lost to wraparound, an uncorroborated root, or a legacy record
// with no parent field — and the caller keeps the bare name.
func resolvePath(byID map[uint64]objEntry, root uint64, haveRoot bool, name string, parent uint64) (string, bool) {
	if name == "" || parent == 0 {
		return "", false
	}
	parts := []string{name} // deepest first; ancestors are prepended, so parts[0] ends up root-most
	cur := parent
	for range maxPathDepth {
		if haveRoot && cur == root {
			return strings.Join(parts, "/"), true
		}
		ent, ok := byID[cur]
		if !ok {
			return "", false // ancestor's create is outside the window (or a cycle hit the depth cap)
		}
		parts = append([]string{ent.name}, parts...)
		cur = ent.parent
	}
	return "", false
}

// eventMatchesKey reports whether event i is history for the full S3 key.
//
// Fully-resolved events match on exact Key/OldKey equality. Partial
// events (parent chain unresolvable: records lost to wraparound, or
// legacy records with no parent field) keep their bare name and match
// conservatively: exact bare-name equality, or the queried key ending in
// "/"+bare. Showing a possibly-unrelated event beats silently hiding the
// only record of an object — the exact F-live-1 failure mode.
func eventMatchesKey(set *eventSet, i int, key string) bool {
	e := set.events[i]
	if set.resolved[i] {
		if e.Key == key {
			return true
		}
	} else if bareMatchesKey(e.Key, key) {
		return true
	}
	if e.OldKey != "" {
		if set.oldResolved[i] {
			if e.OldKey == key {
				return true
			}
		} else if bareMatchesKey(e.OldKey, key) {
			return true
		}
	}
	return false
}

func bareMatchesKey(bare, key string) bool {
	return bare == key || strings.HasSuffix(key, "/"+bare)
}

// truncateForErr keeps error strings bounded on garbage output.
func truncateForErr(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// zfsEventsProvider is the "zfs-events" MetadataProvider: it serves
// per-object event history from the dataset ring buffer via
// `zfs events -j <dataset>` and clears it via `zfs events -c <dataset>`.
//
// detail carries the HistoryDetail (dataset + recordsLost) of the most
// recent completed History call ON THIS INSTANCE. The MetadataProvider
// interface is frozen (must not gain methods), so consumers read this via
// the frontend's structural detailReporter interface (LastDetail) —
// per-instance state, unlike the test-only package global, is safe under
// concurrent History calls on different buckets (bughunt A2/C2).
type zfsEventsProvider struct {
	detailMu sync.Mutex
	detail   HistoryDetail
}

// LastDetail returns the detail of the most recent completed History call
// on this provider instance (the frontend's detailReporter seam).
func (p *zfsEventsProvider) LastDetail() HistoryDetail {
	p.detailMu.Lock()
	defer p.detailMu.Unlock()
	return p.detail
}

func (p *zfsEventsProvider) setDetail(d HistoryDetail) {
	p.detailMu.Lock()
	defer p.detailMu.Unlock()
	p.detail = d
}

// NewZFSEventsProvider returns the "zfs-events" provider.
func NewZFSEventsProvider() MetadataProvider { return &zfsEventsProvider{} }

func (p *zfsEventsProvider) Name() string { return "zfs-events" }

// History returns events for the dataset behind bucketPath, scoped to key
// when key is non-empty. Ring buffers return newest-first; MaxEvents caps
// post-filter. Since filters on Timestamp — events with a zero timestamp
// (hrtime absent on the wire) always pass, because "unknown time" is not
// "older than Since". Records lost to ring-buffer wraparound are surfaced
// via the provider instance's LastDetail (detailReporter seam) immediately
// after this call returns.
func (p *zfsEventsProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
	ds, err := resolveDatasetFn(ctx, bucketPath)
	if err != nil {
		return nil, err
	}
	args := []string{"events", "-j"}
	// MaxEvents*3 fetch headroom: the CLI cannot filter by key, so the
	// key filter happens post-parse; without headroom a key-scoped query
	// would silently return fewer than MaxEvents events. We do NOT use
	// the upstream `-o <object-id>` fast path: it filters by
	// stat().st_ino, which equals the ZFS object id on Linux but is not
	// guaranteed on other platforms, and proving the mapping needs a
	// real-host integration test (see leaf Notes).
	if q.MaxEvents > 0 {
		args = append(args, "-n", strconv.Itoa(q.MaxEvents*3))
	}
	args = append(args, ds)
	stdout, stderr, err := runZFS(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("metadata: zfs events %s: %w (stderr: %s)", ds, err, stderr)
	}
	set, lost, err := parseEventSet(string(stdout))
	if err != nil {
		return nil, err
	}
	events := set.events
	// Per-instance detail (bughunt A2/C2): the endpoint reads this via the
	// detailReporter interface, so concurrent ?events on different buckets
	// can no longer cross-attribute dataset/recordsLost. The package
	// global (setHistoryDetail) stays updated for test compatibility.
	d := HistoryDetail{Dataset: ds, RecordsLost: lost}
	p.setDetail(d)
	setHistoryDetail(d)

	// Key filter runs BEFORE the Since filter: eventMatchesKey indexes
	// into the pristine set, and the Since pass re-slices `events`,
	// which would desync the indices.
	if key != "" {
		filtered := make([]ObjectEvent, 0, len(events))
		for i, e := range events {
			if eventMatchesKey(set, i, key) {
				filtered = append(filtered, e)
			}
		}
		events = filtered
	}
	if !q.Since.IsZero() {
		filtered := events[:0]
		for _, e := range events {
			if e.Timestamp.After(q.Since) || e.Timestamp.IsZero() {
				filtered = append(filtered, e) // zero timestamps pass: unknown, not old
			}
		}
		events = filtered
	}
	if q.MaxEvents > 0 && len(events) > q.MaxEvents {
		events = events[:q.MaxEvents]
	}
	return events, nil
}

// probeCore is the GOOS-independent heart of Probe: resolve the dataset,
// then check the dataset's `events` property is "on". The dataset name is
// passed in (resolved via resolveDatasetFn by Probe) so tests can drive
// this path on hosts without statfs ZFS detection. Every failure mode
// degrades to Available=false + Reason with a nil error — "not available"
// is a status, not a hard failure.
func (p *zfsEventsProvider) probeCore(ctx context.Context, bucketPath, dataset string) (ProbeResult, error) {
	_ = bucketPath
	out, stderr, err := runZFS(ctx, "get", "-H", "-o", "value", "events", dataset)
	if err != nil {
		return ProbeResult{Available: false, Reason: "feature check: " + err.Error() + ": " + stderr}, nil //nolint:nilerr // unavailable is a ProbeResult status, not a failure (contract comment above)
	}
	if strings.TrimSpace(string(out)) != "on" {
		return ProbeResult{Available: false, Reason: "dataset property events is not 'on'", Dataset: dataset}, nil
	}
	return ProbeResult{Available: true, Dataset: dataset}, nil
}

// Probe reports whether the bucket path sits on a ZFS dataset with the
// events feature enabled. It never returns a non-nil error for
// "unavailable" — that is ProbeResult{Available:false, Reason}.
func (p *zfsEventsProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
	resolved, err := filepath.EvalSymlinks(bucketPath)
	if err != nil {
		return ProbeResult{Available: false, Reason: "path resolve: " + err.Error()}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	isZFS, err := DetectZFS(resolved)
	if err != nil {
		return ProbeResult{Available: false, Reason: "statfs: " + err.Error()}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	if !isZFS {
		return ProbeResult{Available: false, Reason: "filesystem is not ZFS"}, nil
	}
	ds, err := resolveDatasetFn(ctx, resolved)
	if err != nil {
		return ProbeResult{Available: false, Reason: "dataset resolve: " + err.Error()}, nil //nolint:nilerr // unavailable is a status, not a failure
	}
	return p.probeCore(ctx, resolved, ds)
}

// Purge clears the dataset's event ring buffer via `zfs events -c`. It is
// destructive: endpoint wiring (leaf 04) must keep it authenticated and
// server startup paths must never call it.
func (p *zfsEventsProvider) Purge(ctx context.Context, bucketPath string) error {
	ds, err := resolveDatasetFn(ctx, bucketPath)
	if err != nil {
		return err
	}
	_, stderr, err := runZFS(ctx, "events", "-c", ds)
	if err != nil {
		return fmt.Errorf("metadata: zfs events -c %s: %w (stderr: %s)", ds, err, stderr)
	}
	return nil
}
