# Design: Cached / Batched Meta Reads for ListObjectsV2

Status: DESIGN ONLY - not implemented in mini-s3. Written 2026-09-28 as a
reference for implementation elsewhere. Author: hardening-campaign bench
findings (leaf 4.10).

## Problem, measured

`BenchmarkListObjectsFromKeysPlain` on Apple M2 Ultra:
- 10k keys, page of 1000: **14.96 ms/op, 1.41 MB/op, 10018 allocs/op**
- Same walk with delimiter roll-up: **0.19 ms/op, 11 KB/op, 15 allocs**
- Delimiter is ~80x cheaper because it stops after 100 roll-ups and never
  reads the 1000 per-key `.meta` files.

The cost is not the key walk (`filepath.WalkDir` is cheap). It is
`os.ReadFile` + `json.Unmarshal` PER KEY per LIST request - 1000 of each for
a full page, every time anyone lists. `BenchmarkListObjectsFromKeysPrefix`
confirms: prefix filtering does not avoid the meta reads for keys that pass
the filter.

Storage layout this design assumes (mini-s3's, but the pattern is general):
```
<bucket>/
  <object-data-files...>
  .metadata/
    <key>.meta          # one JSON file per object:
                        # {contentType, contentLength, etag, customMetadata,
                        #  lastModified, storagePath}
```

## Design options, in ascending complexity

### Option A - batch meta reads (no new state, immediate 2-5x)

Read all page keys' meta files concurrently instead of serially. Worker pool
of `runtime.NumCPU()` (bounded, e.g. 8-32) goroutines; results collected into
a pre-sized slice. Meta files are small (<1KB) and on the same filesystem, so
concurrent reads scale until the disk saturates.

```go
type metaResult struct {
    idx  int
    meta ObjectMetadata
    err  error
}
func readMetasBatch(paths []string, workers int) []ObjectMetadata {
    results := make([]ObjectMetadata, len(paths))
    ch := make(chan metaResult, len(paths))
    sem := make(chan struct{}, workers)
    var wg sync.WaitGroup
    for i, p := range paths {
        wg.Add(1)
        go func(idx int, path string) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()
            b, err := os.ReadFile(path)
            if err != nil { ch <- metaResult{idx, ObjectMetadata{}, err}; return }
            var m ObjectMetadata
            err = json.Unmarshal(b, &m)
            ch <- metaResult{idx, m, err}
        }(i, p)
    }
    wg.Wait(); close(ch)
    for r := range ch {
        if r.err == nil { results[r.idx] = r.meta }  // failed reads: skip key
    }
    return results
}
```
- Expected gain: 3-5x on SSD (14.96ms → ~3-5ms for 1000 keys); more on NVMe.
- Correctness: unchanged - same files, same parse, just parallel.
- Failure semantics: keep mini-s3's "skip keys whose meta is unreadable"
  behavior per-key.
- Effort: ~50 lines + tests. This is the recommended first step everywhere.

### Option B - append-only meta journal + snapshot cache

Kill the per-key meta READ on the list path entirely. Keep the per-key `.meta`
files as the durable truth (back-compat, single-object GET path unchanged),
but maintain an in-memory index rebuilt at startup and updated on writes.

```
In-memory: map[key]ObjectMetaEntry   (key → etag, size, lastModified,
                                      contentType; NOT storagePath — derive)
Mutation points (all already behind per-key locks):
  PUT / multipart complete  → index.Store(key, entry)
  DELETE / batch delete     → index.Delete(key)
  startup                   → WalkDir .metadata, load all (parallel)
Snapshot for listing: copy the map under RLock (COW or epoch-based), sort
keys from the snapshot, apply prefix/delimiter/maxKeys in memory. NO file
I/O on the list path at all.
```
- Expected gain: listing becomes pure memory: ~1-2ms for 10k keys regardless
  of page size; allocations drop to page-size only.
- Correctness risks to design around:
  1. **Two sources of truth.** The journal must be updated in the SAME
     critical section as the meta-file write, AFTER it succeeds (write-file
     wins; index mirrors disk). If they diverge (crash between file write and
     index update), startup rebuild heals it - index is always a CACHE, never
     authoritative.
  2. **External file mutation.** mini-s3 supports custom bucket dirs that
     other processes write. An index without invalidation goes stale there.
     Mitigations: (a) fsnotify/watchdog on .metadata dirs, (b) TTL re-walk,
     (c) only index dataDir buckets, walk custom buckets per-request
     (Option A batch reads there). Recommend (c) + (a) as a follow-up.
  3. **Memory.** 1M objects x ~200B entry ≈ 200MB - fine for this class of
     server; add a max-objects config with fallback to Option A per-page
     reads above the limit.
  4. **Startup cost.** 1M keys ≈ parallel read of 1M small files: seconds.
     Do it lazily: serve lists from partial index while a background loader
     fills it (list correctness: snapshot may be incomplete → document, or
     block ListBuckets until loaded).
- Effort: ~300-500 lines: index type, hook points in 5 write paths, loader,
  tests. The hook points already all exist and are locked (storage.go
  writeFileAtomic call sites) - which is what makes this tractable.

### Option C - single-file meta (schema change, NOT recommended for mini-s3)

Replace per-key files with one append-only log + periodic compaction (like
SQLite/WAL). Fastest, but breaks the custom-bucket-with-existing-files use
case and every tool that reads `.metadata/` directly. Only worth it if you
control the whole storage format.

## Recommendation

Implement A first (universal, no invalidation problem), measure, then B only
if listing is still hot. The hardening campaign's locking (per-key +
parent-dir locks, bucket lock) means all mutation points are serialized per
key already - B's hook points are exactly those call sites.

## Test contract any implementation must satisfy

1. List output byte-identical before/after (golden: one list per feature
   shape, captured pre-change).
2. Meta file unreadable for one key → that key skipped, rest served (Option A
   and B startup).
3. Concurrent PUT+LIST under `-race`: list never sees a torn entry (index
   snapshot or per-file atomic reads guarantee this; the meta files are
   written via temp+rename already).
4. External mutation of a .meta file: Option B must heal within its stated
   invalidation policy - test with a direct os.WriteFile between lists.
5. Startup with 0 / 1 / N buckets, corrupt journal entries skipped.

## Benchmarks to prove the win

Keep `BenchmarkListObjectsFromKeys{Plain,Delimiter,Prefix}` (already in
bench_test.go) as the before/after ruler; add
`BenchmarkListObjectsV2Handler` end-to-end through the handler if Option B
lands (the walk disappears entirely there).
