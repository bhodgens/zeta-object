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
func parseEventsOutput(out string) ([]ObjectEvent, uint64, error) {
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
	events := make([]ObjectEvent, 0, len(raws))
	for _, r := range raws {
		op := strings.ToLower(r.Op)
		e := ObjectEvent{
			Op:  op,
			Txg: r.Txg,
		}
		if r.Name != nil {
			e.Key = *r.Name
		}
		if r.OldName != nil {
			e.OldKey = *r.OldName
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
		events = append(events, e)
	}
	return events, lost, nil
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
	events, lost, err := parseEventsOutput(string(stdout))
	if err != nil {
		return nil, err
	}
	// Per-instance detail (bughunt A2/C2): the endpoint reads this via the
	// detailReporter interface, so concurrent ?events on different buckets
	// can no longer cross-attribute dataset/recordsLost. The package
	// global (setHistoryDetail) stays updated for test compatibility.
	d := HistoryDetail{Dataset: ds, RecordsLost: lost}
	p.setDetail(d)
	setHistoryDetail(d)

	if !q.Since.IsZero() {
		filtered := events[:0]
		for _, e := range events {
			if e.Timestamp.After(q.Since) || e.Timestamp.IsZero() {
				filtered = append(filtered, e) // zero timestamps pass: unknown, not old
			}
		}
		events = filtered
	}
	if key != "" {
		filtered := events[:0]
		for _, e := range events {
			if e.Key == key || e.OldKey == key {
				filtered = append(filtered, e)
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
