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
