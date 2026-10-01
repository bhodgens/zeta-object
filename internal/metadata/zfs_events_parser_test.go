package metadata

// zfs_events_parser_test.go - TEST-ONLY legacy zfs-events CLI (`-j`) fixture
// parser (moved out of zfs_events.go by zmetad-provider-2026-09 leaf 04).
//
// The production CLI transport is deleted: nothing here is linked into
// the server binary. The parser survives ONLY because the leaf-02 drift
// guard (TestRowsToEventsDriftGuardFixtures in zmetad_paths_test.go)
// consumes parseEventSet + rawEvent + the recorded testdata fixtures to
// pin that the v5 full_path fast path (rowsToEvents) produces output
// identical to the historical CLI reconstruction on every fixture. When
// the fixtures and the drift guard are retired, delete this file.
//
// Retained symbols (all test-only): rawEvent, recordsLostRe,
// parseEventsOutput, parseEventSet, eventSet, objEntry, res,
// maxPathDepth, reconstructPaths, resolveRecordAt, mapAt,
// anyObjIDMappedTwice, detectRoot, resolvePath, truncateForErr.
// Deleted (no test consumer after the CLI-provider tests died):
// eventMatchesKey. plausibleWallClockTime and bareMatchesKey STAY in
// production (zfs_events.go) - zmetad_paths.go uses them.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

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

// parseEventsOutput parses legacy `zfs` events CLI (`-j`) stdout: a JSON array optionally
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
		return nil, 0, fmt.Errorf("metadata: zfs-events CLI output has no JSON array: %q", truncateForErr(out))
	}
	var raws []rawEvent
	if err := json.Unmarshal([]byte(out[:end+1]), &raws); err != nil {
		return nil, 0, fmt.Errorf("metadata: parsing zfs-events CLI JSON: %w", err)
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
// chain is complete (F-live-1: the legacy events CLI emits bare names only —
// verified against print_event, zfs_main.c:8332, which never emits paths).
//
// Graph rules pinned from the live zfs-meta host (OpenZFS 2.4.1
// extended-metadata branch, fixture zfs-events-live-ordcap.txt):
//   - every record carries "object"; CREATE/LINK/RENAME/REMOVE records
//     carry "name" and (when nonzero) "parent" = containing dir object id;
//   - RENAME: name = new bare name, old_name = old bare name, parent =
//     old_parent = the (unchanged) parent dir object id;
//   - directory CREATEs appear too, with their own object id — that is
//     the graph walked here;
//   - the dataset-root dir emits its own record on ordinary activity
//     (observed: a SETATTR with no name and no parent, object=34) — such
//     nameless records are skipped by the map build and never elect a
//     phantom root;
//   - records are OLDEST-FIRST (txg ascending) — the live capture pinned
//     this (bughunt M8), so mappings are txg-scoped: each record resolves
//     against the mapping state as of its own stream position. Object-id
//     reuse and in-window directory renames therefore relabel only going
//     forward; pre-event records keep their historical (pre-rename /
//     pre-reuse) paths.
//
// res carries per-record path-resolution verdicts for reconstructPaths.
type res struct {
	key, oldKey  string
	keyOK, oldOK bool
}

// resolveRecordAt resolves record i's own names against the mapping state
// as of its position (pre-update state: a CREATE of dir X must not resolve
// through X itself, and a RENAME's new name must not shortcut through a
// mapping this same record creates), storing the verdicts in resolved.
func resolveRecordAt(raws []rawEvent, i int, fullMap map[uint64]objEntry, resolved []res) {
	r := raws[i]
	if r.Name == nil || r.Object == 0 {
		return
	}
	op := strings.ToLower(r.Op)
	byID := mapAt(raws, i, fullMap)
	// print_event emits `parent` alongside `old_parent` for RENAME
	// (same dir), but tolerate its absence.
	parent := r.Parent
	if parent == 0 && op == "rename" {
		parent = r.OldParent
	}
	root, haveRoot := detectRoot(raws, fullMap)
	if full, ok := resolvePath(byID, root, haveRoot, *r.Name, parent); ok {
		resolved[i].key, resolved[i].keyOK = full, true
	}
	if r.OldName != nil {
		oldParent := r.OldParent
		if oldParent == 0 {
			oldParent = r.Parent
		}
		if full, ok := resolvePath(byID, root, haveRoot, *r.OldName, oldParent); ok {
			resolved[i].oldKey, resolved[i].oldOK = full, true
		}
	}
}

// mapAt returns the mapping state as of record i (records [0, i) applied).
// O(n^2) worst case only in the reuse window; windows are bounded by
// MaxEvents*3.
func mapAt(raws []rawEvent, i int, fullMap map[uint64]objEntry) map[uint64]objEntry {
	anyReuse := anyObjIDMappedTwice(raws)
	if !anyReuse {
		return fullMap
	}
	m := make(map[uint64]objEntry, i)
	for _, r := range raws[:i] {
		if r.Name == nil || r.Object == 0 {
			continue
		}
		parent := r.Parent
		if parent == 0 && strings.ToLower(r.Op) == "rename" {
			parent = r.OldParent
		}
		m[r.Object] = objEntry{name: *r.Name, parent: parent}
	}
	return m
}

// anyObjIDMappedTwice reports whether any object id appears in two records
// (rename relabel of a known dir, or id reuse).
func anyObjIDMappedTwice(raws []rawEvent) bool {
	seen := make(map[uint64]bool, len(raws))
	for _, r := range raws {
		if r.Name == nil || r.Object == 0 {
			continue
		}
		if seen[r.Object] {
			return true
		}
		seen[r.Object] = true
	}
	return false
}

func reconstructPaths(raws []rawEvent) *eventSet {
	set := &eventSet{
		events:      make([]ObjectEvent, 0, len(raws)),
		resolved:    make([]bool, len(raws)),
		oldResolved: make([]bool, len(raws)),
	}
	// Pass 1: txg-scoped (historical) map build + resolution (bughunt H6,
	// M8). The stream is OLDEST-FIRST (txg ascending — pinned by the live
	// ordcap fixture), so the walk is in causal order:
	//
	//   - a record is RESOLVED against the mapping state AS OF ITS OWN
	//     POSITION (the map built from records [0, i)): a CREATE of dir X
	//     cannot resolve through itself, a RENAME resolves both names
	//     through the pre-rename map, and objid reuse relabels only
	//     FORWARD — earlier records keep their historical (pre-rename /
	//     pre-reuse) paths;
	//   - root detection runs against the FULL-WINDOW map. The whole
	//     stream came from ONE CLI ring-buffer read (one immutable buffer
	//     snapshot), so the full map is the state a replaying reader holds
	//     before interpreting any record. Corroboration is therefore
	//     window-wide, not prefix-wide: resolving the nested fixture's
	//     root-level top.txt create against the thin prefix map would
	//     mark PARTIAL an event the buffer itself fully corroborates.
	//     History is never rewritten: the per-position map controls which
	//     NAMES resolve, the full map only controls WHERE chains stop.
	//
	// When no objid is mapped twice (the common window: no in-window dir
	// rename, no reuse), every position sees the same map — the final map
	// directly, no copies. A doubled id (rename relabel / reuse) triggers
	// per-record prefix replay for correct pre/post-image resolution.
	resolved := make([]res, len(raws))
	fullMap := make(map[uint64]objEntry, len(raws))
	for _, r := range raws {
		if r.Name == nil || r.Object == 0 {
			continue
		}
		if _, dup := fullMap[r.Object]; !dup {
			fullMap[r.Object] = objEntry{name: *r.Name, parent: r.Parent}
		}
	}
	for i := range raws {
		resolveRecordAt(raws, i, fullMap, resolved)
	}
	// Pass 2: materialize events with the pass-1 resolution verdicts.
	for i, r := range raws {
		op := strings.ToLower(r.Op)
		e := ObjectEvent{Op: op, Txg: r.Txg}
		if r.Name != nil {
			e.Key = *r.Name
			if resolved[i].keyOK {
				e.Key = resolved[i].key
				set.resolved[i] = true
			}
		}
		if r.OldName != nil {
			e.OldKey = *r.OldName
			if resolved[i].oldOK {
				e.OldKey = resolved[i].oldKey
				set.oldResolved[i] = true
			}
		}
		// Sizes are only meaningful for truncate on the wire (print_event
		// emits them unconditionally only in that branch).
		if op == "truncate" {
			e.SizeOld, e.SizeNew = r.OldSize, r.NewSize
		}
		// Timestamp is set only for plausible wall-clock values (bughunt
		// M2): boot-relative hrtime stays zero (= unknown on the wire).
		e.Timestamp = plausibleWallClockTime(r.TimeNs)
		e.UID, e.GID = uint32(r.UID), uint32(r.GID) //nolint:gosec // G115: uid/gid are 32-bit on every platform the zfs event log reports; truncation matches zfs behavior
		set.events = append(set.events, e)
	}
	return set
}

// detectRoot identifies the dataset root directory's object id so
// parent-chain walks can terminate. The root predates the ring buffer, so
// its id usually never appears as an "object" — only as a "parent" of
// top-level entries. (The live fixture shows the root CAN emit its own
// nameless SETATTR record; nameless records never enter the object map, so
// they cannot perturb detection here — bughunt H5.)
//
// A wrong root claim fabricates wrong EXACT keys (the F-live-1
// silent-empty failure in reverse), so detection is strict:
//
//  1. Only KNOWN directories vote — object ids that appear in the object
//     map (a named record for that id is in-window) and are referenced as
//     some record's parent.
//  2. Each known dir climbs its own resolvable ancestor chain; the first
//     id ABOVE the chain (absent from the object map) is that dir's root
//     candidate — the root, or an ancestor lost to wraparound.
//  3. CHAIN-TERMINUS UNANIMITY (bughunt H4 fix): raw frequency voting is
//     WRONG — a lost mid-chain directory referenced by many children
//     out-votes the true root and every "resolved" key silently drops path
//     components. Instead, the elected root must be the terminus of EVERY
//     resolvable voting chain. If any voting chain terminates at a
//     different id, detection is REFUSED and all events degrade to partial
//     (conservative bare-name matching) — a wrong exact key is worse than
//     a broad match. Ties cannot occur under unanimity; the smallest
//     candidate id wins for determinism if a single chain yields several
//     (impossible in practice).
//
// With no known directory in the window (entire window inside a dir
// whose create was lost) detection is refused: everything stays partial
// and matches conservatively.
func detectRoot(raws []rawEvent, byID map[uint64]objEntry) (uint64, bool) {
	// Parent references in stream order; a record that references the same
	// parent twice counts once (votes are per-chain, not per-reference).
	isParent := make(map[uint64]bool)
	for _, r := range raws {
		if r.Name == nil {
			continue // nameless records (e.g. the root's own SETATTR) never vote
		}
		if r.Parent != 0 {
			isParent[r.Parent] = true
		}
		if strings.ToLower(r.Op) == "rename" && r.OldParent != 0 {
			isParent[r.OldParent] = true
		}
	}
	// One terminus candidate per KNOWN directory that is referenced as a
	// parent (i.e. has children voting through it).
	var candidates []uint64
	for id := range isParent {
		ent, ok := byID[id]
		if !ok {
			continue // referenced dir whose create is outside the window
		}
		// Climb this dir's resolvable ancestor chain; the id above it
		// is the root candidate. Depth-capped against cycles from
		// wraparound object-id reuse.
		cand := uint64(0)
		for range maxPathDepth {
			next, ok := byID[ent.parent]
			if !ok {
				cand = ent.parent // 0 = chain reached a parent-less root record shape
				break
			}
			ent = next
		}
		candidates = append(candidates, cand)
	}
	if len(candidates) == 0 {
		return 0, false
	}
	// Unanimity: every voting chain must terminate at the SAME id above
	// the map. A dissenting chain means an ancestor ambiguity we cannot
	// resolve safely — refuse detection entirely (bughunt H4).
	first := candidates[0]
	if first == 0 {
		return 0, false // a known dir with parent 0/lost: no resolvable terminus
	}
	for _, c := range candidates[1:] {
		if c != first {
			return 0, false
		}
	}
	return first, true
}

// resolvePath reconstructs the full S3 key for a bare name under the
// directory object id parent by walking parent -> grandparent until the
// dataset root. ok=false means the chain could not be fully resolved —
// records lost to wraparound, an uncorroborated root, or a legacy record
// with no parent field — and the caller keeps the bare name.
//
// A parent EQUAL to the elected root resolves to a root-level key (the
// election is chain-terminus-unanimous, so this cannot fabricate a prefix;
// bughunt H4). With detection refused (haveRoot=false) nothing resolves
// through the root and every record whose chain leaves the map stays
// partial.
func resolvePath(byID map[uint64]objEntry, root uint64, haveRoot bool, name string, parent uint64) (string, bool) {
	if name == "" {
		return "", false
	}
	parts := []string{name} // deepest first; ancestors are prepended, so parts[0] ends up root-most
	cur := parent
	for range maxPathDepth {
		if haveRoot && cur == root {
			return strings.Join(parts, "/"), true
		}
		if cur == 0 {
			return "", false // legacy record with no parent field
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

// truncateForErr keeps error strings bounded on garbage output.
func truncateForErr(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
