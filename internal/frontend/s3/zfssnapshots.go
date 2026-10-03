// zfssnapshots.go — ZFS-snapshot-backed faux versioning (s3-versioning
// tree leaf 02, Contract 3 + Contract 1's zfsSnapshotVersionStore).
//
// ZFS buckets default to FAUX VERSIONING derived from the dataset's
// EXISTING snapshots: no snapshots are taken, no delete markers exist,
// and the versionId IS the snapshot short name (the durable handle while
// the snapshot exists). Enumeration is ONE `zfs list -H -t snapshot
// -o name,creation -d 1 <dataset>` exec, semaphore-bounded, 5s timeout,
// argv-only; per-(snapshot, key) existence is a stat under
// <mountpoint>/.zfs/snapshot/<snapShort>/<relPath> (snapdirs are hidden
// but directly accessible). Reads NEVER substitute current data: an
// unknown/expired snapshot or a key missing in that snapshot is honest
// NoSuchKey (404-class), because the snapshot window is the only truth
// this store exposes.
//
// Charter: enumeration is read-only exec; the bucket's versioning STATE
// reuses the same bucket-level sidecar marker as the sidecar store
// (bucket-level state is path- and mechanism-independent). No
// server-owned identity state is introduced. v1 requires zmetad
// tracking (dataset + mountpoint from the `datasets` table) — the same
// prerequisite as ?events; ResolveDataset-style fallback is NOT used.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// ErrSnapshotsReadOnly is returned by PutVersion on the ZFS snapshot
// store: snapshots are a policy the HOST owns, so versioned writes are
// not possible (enable sidecar or both mode for true per-write
// versioning).
var ErrSnapshotsReadOnly = errors.New("s3: zfs snapshot versioning is read-only")

// zfsSnapCmdTimeout bounds the single snapshot-enumeration exec.
const zfsSnapCmdTimeout = 5 * time.Second

// zfsSnapExecSem bounds concurrent zfs processes from this store (the
// zfsExecSem discipline, bug-hunt D5): every ?versions read on a
// snapshots-mode bucket can exec zfs, so an authenticated client
// fan-out must not spawn an unbounded number of concurrent processes.
// A dedicated channel keeps this package's bound independent of the
// deleted metadata-package semaphore.
var zfsSnapExecSem = make(chan struct{}, 4)

// zfsSnapshotRunner is the seam tests replace (the old zfsRunner
// pattern). Production execs the zfs CLI: argv-only (never a shell
// string — the dataset name comes from the zmetad datasets table, never
// user input), context timeout, stderr captured.
var zfsSnapshotRunner = func(ctx context.Context, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsSnapCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zfs", args...) // #nosec G204 // argv-only; dataset names come from the zmetad DB, never user input
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// runZfsSnapshot bounds the exec through zfsSnapExecSem, honoring ctx:
// a request that already timed out never queues behind the bound.
func runZfsSnapshot(ctx context.Context, args ...string) ([]byte, string, error) {
	select {
	case zfsSnapExecSem <- struct{}{}:
		defer func() { <-zfsSnapExecSem }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return zfsSnapshotRunner(ctx, args...)
}

// SnapInfo is one dataset snapshot (Contract 3).
type SnapInfo struct {
	// Name is the full dataset@snapshot form (e.g. pool/bkt@auto-20261002).
	Name string
	// Short is the snapshot name alone (the versionId in snapshots mode).
	Short string
	// Creation is the snapshot creation time (zero when unparseable).
	Creation time.Time
}

// ListSnapshots enumerates the dataset's snapshots NEWEST FIRST
// (Contract 3): ONE `zfs list -H -t snapshot -o name,creation -d 1
// <dataset>` exec, semaphore-bounded, 5s timeout, argv-only. Blank or
// malformed lines are skipped (some zfs builds pad output); a snapshot
// with an unparseable creation keeps a zero time rather than being
// dropped (the name is still a valid read handle). zfs's default sort is
// by name — the result is explicitly sorted by creation.
func ListSnapshots(ctx context.Context, dataset string) ([]SnapInfo, error) {
	stdout, stderr, err := runZfsSnapshot(ctx, "list", "-H", "-t", "snapshot", "-o", "name,creation", "-d", "1", dataset)
	if err != nil {
		return nil, fmt.Errorf("s3: zfs list snapshots %s: %w (stderr: %s)", dataset, err, stderr)
	}
	return parseZfsSnapshotList(stdout), nil
}

// parseZfsSnapshotList parses `zfs list -H -t snapshot -o name,creation`
// stdout into newest-first SnapInfo values. Test-only surface via
// ListSnapshots; exported nowhere.
func parseZfsSnapshotList(stdout []byte) []SnapInfo {
	var snaps []SnapInfo
	for line := range strings.SplitSeq(string(stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Row shape: <full name>	<creation> — the full name is
		// dataset@snapshot (split on the LAST '@': dataset parts rarely
		// but legally contain '@'), creation is everything after the
		// first TAB (formatted creations contain spaces, so tab is the
		// separator and the remainder is the token verbatim).
		tab := strings.IndexByte(line, '	')
		if tab < 0 {
			continue
		}
		full := strings.TrimSpace(line[:tab])
		idx := strings.LastIndex(full, "@")
		if idx < 0 || idx == len(full)-1 {
			continue // not a dataset@snapshot row (or bare '@' tail)
		}
		creationTok := strings.TrimSpace(line[tab+1:])
		info := SnapInfo{
			Name:     full,
			Short:    full[idx+1:],
			Creation: parseZfsCreation(creationTok),
		}
		snaps = append(snaps, info)
	}
	// zfs's default list order is by name — sort explicitly by creation,
	// newest first (stable: equal timestamps keep zfs's name order).
	sort.SliceStable(snaps, func(i, j int) bool {
		return snaps[i].Creation.After(snaps[j].Creation)
	})
	return snaps
}

// parseZfsCreation parses `zfs list -o creation` output: the epoch form
// (`zfs list -p`) and the locale `date` form both occur; anything else
// is a zero time (the name remains a valid read handle).
func parseZfsCreation(tok string) time.Time {
	if epoch, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return time.Unix(epoch, 0).UTC()
	}
	// `zfs list` (non -p) formats creation with ctime(): "%a %b %e
	// %H:%M %Y" — MINUTE resolution, no seconds ("Mon Oct  2 17:30
	// 2026", single-digit hours space-padded). Both the _2 and double-
	// space day forms are accepted; year-only locale variations
	// degrade to zero. (Found by the zfs-validate section 10 probe:
	// zfs-meta's zfs 2.4.1 emits exactly this form, and the seconds
	// layouts parsed it to zero — collapsing newest-first ordering to
	// zfs name order.)
	if ts, err := time.Parse("Mon Jan _2 15:04 2006", tok); err == nil {
		return ts.UTC()
	}
	if ts, err := time.Parse("Mon Jan  2 15:04 2006", tok); err == nil {
		return ts.UTC()
	}
	if ts, err := time.Parse("Mon Jan _2 15:04:05 2006", tok); err == nil {
		return ts.UTC()
	}
	if ts, err := time.Parse("Mon Jan  2 15:04:05 2006", tok); err == nil {
		return ts.UTC()
	}
	return time.Time{}
}

// SnapPath is the pure path helper (Contract 3):
// <mountpoint>/.zfs/snapshot/<snapShort>/<relPath>.
func SnapPath(mountpoint, snapShort, relPath string) string {
	return filepath.Join(mountpoint, ".zfs", "snapshot", snapShort, filepath.FromSlash(relPath))
}

// datasetResolver is the surface this store consumes from the zmetad
// database. *metadata.ZmetadDB implements it; tests stub it.
type datasetResolver interface {
	ResolveDatasetByPath(path string) (string, error)
	ResolveMountpointByPath(path string) (string, error)
}

// compile-time assertion: the real zmetad DB satisfies the resolver seam.
var _ datasetResolver = (*metadata.ZmetadDB)(nil)

// zfsSnapshotVersionStore implements versionStore over the bucket
// dataset's EXISTING snapshots (Contract 1, snapshots mode). bucketPath
// must be the symlink-resolved bucket root; zdb resolves it to
// dataset + mountpoint via the zmetad datasets table.
type zfsSnapshotVersionStore struct {
	bucketPath string
	zdb        datasetResolver
}

// resolve maps the bucket path to (dataset, mountpoint). zmetad tracking
// is the v1 prerequisite for snapshots mode (same as ?events): an
// untracked bucket is a hard error, never a silent degradation to
// current-data reads.
func (s zfsSnapshotVersionStore) resolve() (dataset, mountpoint string, err error) {
	dataset, err = s.zdb.ResolveDatasetByPath(s.bucketPath)
	if err != nil {
		return "", "", fmt.Errorf("s3: resolving dataset for %s: %w", s.bucketPath, err)
	}
	mountpoint, err = s.zdb.ResolveMountpointByPath(s.bucketPath)
	if err != nil {
		return "", "", fmt.Errorf("s3: resolving mountpoint for %s: %w", s.bucketPath, err)
	}
	return dataset, mountpoint, nil
}

// State reads the SHARED bucket-level versioning state marker — the
// same <bucket>/.metadata/.versioning file the sidecar store writes.
// Bucket-level state is path- and mechanism-independent.
func (s zfsSnapshotVersionStore) State(bucket string) (string, error) {
	return sidecarVersionStore{bucketPath: s.bucketPath}.State(bucket)
}

// SetState writes the shared bucket-level versioning state marker via
// the sidecar store's implementation (same validation, same file).
func (s zfsSnapshotVersionStore) SetState(bucket, state string) error {
	return sidecarVersionStore{bucketPath: s.bucketPath}.SetState(bucket, state)
}

// PutVersion is unsupported: snapshots are host policy, not server
// writes. ErrSnapshotsReadOnly is typed so handlers (leaf 03) can map
// it to a 409/400-class wire form.
func (s zfsSnapshotVersionStore) PutVersion(bucket, key string, r io.Reader, size int64, etag string) (VersionEntry, error) {
	return VersionEntry{}, ErrSnapshotsReadOnly
}

// PutDeleteMarker is unsupported: a deleted key simply loses future
// snapshot presence; old snapshots keep the data. The sentinel is
// shared with leaf 01 (ErrDeleteMarkersUnsupported).
func (s zfsSnapshotVersionStore) PutDeleteMarker(bucket, key string) (VersionEntry, error) {
	return VersionEntry{}, ErrDeleteMarkersUnsupported
}

// List walks the dataset's snapshots NEWEST FIRST, statting
// <mountpoint>/.zfs/snapshot/<snap>/<relPath> per snapshot and emitting
// an entry for every snapshot containing the key (a windowed history:
// only snapshots that still exist and still hold the key appear). The
// first emitted entry is IsLatest (latest snapshot CONTAINING the key).
// A key with no snapshot presence is objectmodel.ErrNoSuchKey.
func (s zfsSnapshotVersionStore) List(bucket, key string) ([]VersionEntry, error) {
	dataset, mountpoint, err := s.resolve()
	if err != nil {
		return nil, err
	}
	snaps, err := ListSnapshots(context.Background(), dataset)
	if err != nil {
		return nil, err
	}
	relPath := filepath.ToSlash(key)
	var out []VersionEntry
	for _, snap := range snaps {
		p := SnapPath(mountpoint, snap.Short, relPath)
		fi, err := os.Stat(p) //nolint:gosec // G304: path is mountpoint (zmetad DB) + snap short (zfs output) + validated key.
		if err != nil || fi.IsDir() {
			continue // snapshot expired or key absent there: honest window
		}
		out = append(out, VersionEntry{
			ID:           snap.Short,
			IsLatest:     len(out) == 0,
			Size:         fi.Size(),
			LastModified: snap.Creation,
		})
	}
	if len(out) == 0 {
		return nil, objectmodel.ErrNoSuchKey(key)
	}
	return out, nil
}

// Open opens the key's data under the NAMED snapshot. There is no
// current-data substitution anywhere in this path: an unknown/expired
// snapshot id, a traversal-shaped id, or a key absent from that
// snapshot is objectmodel.ErrNoSuchKey (404-class). versionID "" has no
// meaning in snapshots mode (live reads bypass the version store) and
// is NoSuchKey.
func (s zfsSnapshotVersionStore) Open(bucket, key, versionID string) (io.ReadCloser, VersionEntry, error) {
	if versionID == "" || isSnapshotVersionIDUnsafe(versionID) {
		return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
	}
	dataset, mountpoint, err := s.resolve()
	if err != nil {
		return nil, VersionEntry{}, err
	}
	snaps, err := ListSnapshots(context.Background(), dataset)
	if err != nil {
		return nil, VersionEntry{}, err
	}
	var snap *SnapInfo
	for i := range snaps {
		if snaps[i].Short == versionID {
			snap = &snaps[i]
			break
		}
	}
	if snap == nil {
		// Unknown or EXPIRED snapshot: 404 honestly, never fall back to
		// current data.
		return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
	}
	p := SnapPath(mountpoint, snap.Short, filepath.ToSlash(key))
	f, err := os.Open(p) //nolint:gosec // G304: same trusted components as the List stat.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, VersionEntry{}, objectmodel.ErrNoSuchKey(key)
		}
		return nil, VersionEntry{}, fmt.Errorf("s3: opening snapshot version for %s: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // best-effort close on the error path
		return nil, VersionEntry{}, fmt.Errorf("s3: stat snapshot version for %s: %w", key, err)
	}
	entry := VersionEntry{
		ID:           snap.Short,
		Size:         fi.Size(),
		LastModified: snap.Creation,
	}
	return f, entry, nil
}

// isSnapshotVersionIDUnsafe rejects ids that are not clean snapshot
// short names: traversal separators, '.', '..' components, or absolute
// paths (defense in depth — the id is matched against the enumerated
// snapshot list anyway, so nothing unsafe can ever reach SnapPath).
func isSnapshotVersionIDUnsafe(id string) bool {
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return true
	}
	return id == "." || id == ".."
}
