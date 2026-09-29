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
