# testdata — zfs events replay fixtures

## `zfs-events-sample.txt`

Replay fixture for the `zfs-events` metadata provider's parser
(`parseEventsOutput` in `internal/metadata/zfs_events.go`).

**Provenance:** hand-built to match the pinned wire format byte-for-byte
(master Contract 3, verified against the extended-metadata branch source at
`cmd/zfs/zfs_main.c:8332` `print_event`). It was NOT captured from a live
ZFS host; the format fidelity comes from the C source, which is the ground
truth. The file replicates `zfs events -j <dataset>` stdout exactly:

- A JSON array, one compact object per event, comma+newline separated,
  with keys in `print_event` emission order: `txg`, `object`, `op`, then
  `name` (only when present), `parent` (only when nonzero), then
  operation-specific extras (`old_name`/`old_parent` for RENAME,
  `old_size`/`new_size` always for TRUNCATE, `target` for SYMLINK).
- Ops are UPPERCASE on the wire; the parser lowercases into
  `ObjectEvent.Op`.
- A trailing plaintext loss line `N record(s) lost to log wraparound`
  after the closing `]` (printed only when `records_lost > 0`).
- The fixture covers all seven ops of the frozen vocabulary
  (create/remove/rename/link/symlink/truncate/setattr), rename with
  `old_name`+`old_parent`, truncate with sizes (including a
  truncate-to-zero), symlink with `target`, and one wraparound loss line.
  All names are ROOT-LEVEL: with no `parent` fields they stay partial
  events under path reconstruction (see `zfs-events-nested.txt`) and
  match keys exactly as before F-live-1 (backward compatibility pin).

## `zfs-events-nested.txt` (F-live-1)

Nested-path replay fixture replicating the REAL zfs-meta host shapes
(OpenZFS 2.4.1 extended-metadata branch): directory CREATEs with their
own object ids (`edge`=129 under root, `inner`=130 under `edge`), file
creates under them with `parent`=containing-dir objid, a TRUNCATE, a
RENAME (new/old bare names, `parent`=`old_parent`=the unchanged dir
objid), a LINK, and a REMOVE, plus a wraparound loss line. The dataset
root dir appears only as a `parent` reference (objid 34), never as an
`object` — the root predates the ring buffer. This is the fixture the
original nested-key test gap lived in: bare names only ever matched
root-level keys, so `edge/inner/deep.txt` history silently returned [].

## `zfs-events-nested-records-lost.txt` (records-lost variant)

Same shapes, but the window is too narrow: the `inner` dir's create was
lost (its id 777 appears only as an unresolvable parent) and no known
directory corroborates the root. Documented behavior: root detection is
refused, EVERY event stays partial (bare names), and key matching falls
back to conservative bare-name/suffix matching — querying
`edge/deep.txt` still surfaces the create instead of silently returning
[]. A wrong exact key would be worse than a broad match: it silently
hides the only record of an object (the F-live-1 failure mode).

## Reconstruction contract (post-H4/H5/H6/M8 fixes, 2026-09)

The consumer (`reconstructPaths` in `zfs_events.go`) interprets every
fixture under these rules, all pinned by tests:

- OLDEST-FIRST (M8): records are processed in stream (txg) order; the
  per-objid mapping is txg-scoped — each record resolves through the map
  AS OF its own position (H6), so objid reuse and in-window directory
  renames preserve pre-event paths instead of rewriting them.
- Root election (H4): only KNOWN dirs (named records in-window, referenced
  as a parent) vote; each climbs its resolvable ancestor chain to a
  terminus above the map. The election requires CHAIN-TERMINUS UNANIMITY:
  any dissenting terminus refuses detection. Frequency voting is gone (a
  lost mid-chain dir must never out-vote the root).
- Root's own records (H5): the dataset-root dir emits a nameless SETATTR
  on ordinary activity (objid 34 in the ordcap fixture). Nameless records
  never enter the object map and never vote; the root may not be elected
  as terminus "for itself".
- Unresolvable parents / partial contract: when detection is refused (no
  corroboration, or dissent), events keep their BARE names and match keys
  conservatively (exact bare-name or `key` ends with `"/"+bare`). A wrong
  EXACT key is worse than a broad match — the exact key silently hides the
  only record of an object (the F-live-1 failure mode).
- Timestamps (M2): a wire `time` value is mapped to wall-clock only when
  >= 2001-01-01 unix ns; boot-relative hrtime stays zero (= unknown), so
  the Since filter passes those events instead of filtering them all out.

## Regenerating on a real host

On a host running the extended-metadata ZFS branch with a dataset that has
`events=on`:

```sh
zfs events -j <dataset> > internal/metadata/testdata/zfs-events-sample.txt
```

Paste the output verbatim (including the optional trailing loss line). If
the regenerated fixture drops coverage of an op or per-op extra the
current fixture has, extend it — `TestReplayFixtureEndToEnd` requires all
seven ops, rename `old_name`, and truncate sizes to stay covered.

## Real-host integration test

`TestRealZFSSkipsWithoutBinary` (in `zfs_events_test.go`) exercises the
provider against a live dataset when:

- the `zfs` binary is on PATH, and
- `MINIS3_ZFS_TEST_DATASET` names a dataset with `events=on`.

Both conditions are absent on macOS dev hosts, where the test skips.


## zfs-events-live-ordcap.txt (captured 2026-09-30, zfs-meta host)

REAL `zfs events -j testpool/ordcap` output after: mkdir dirA; create+remove
f1.txt; create f2.txt; remove f2.txt; create f3.txt. Key facts it pins:

1. OLDEST-FIRST ordering (txg ascending) - the consumer's per-id mapping
   must be LAST-seen-wins, not first-seen.
2. The dataset-root dir emits its own SETATTR record (object=34, no name,
   no parent) on ordinary activity - root detection must ignore
   nameless records AND must not treat the root as a nameless byID entry.
3. REMOVE records carry name+parent like CREATE.
4. Fresh creates within a mount session get fresh objids; id REUSE requires
   allocator wraparound (long-lived datasets) - mapping rules must still be
   correct when it happens.
