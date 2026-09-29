package s3

// Batched meta reads for ListObjectsV2 (design Option A,
// docs/design/cached-meta-listing.md): bounded-worker concurrent reads of the
// per-key .meta files replace the serial os.ReadFile+json.Unmarshal loop,
// preserving page order, cursor/prefix/delimiter/maxKeys semantics, and the
// skip-unreadable-meta behavior byte-for-byte.

import (
	"encoding/json"
	"os"
	"runtime"
	"sync"
)

// batchMetaReadWorkers bounds the Option A worker pool (design: 8-32).
const batchMetaReadWorkers = 8

// listEntry is a page-eligible object entry: it survived the cursor, prefix
// and delimiter filters, but its metadata has not been read yet. Entries are
// produced in ascending-key order and consumed in that same order by the
// batched reader, so the output page order is identical to the serial walk.
type listEntry struct {
	objectKey string
}

// metaReadResult is the per-path outcome of a batched meta read. Read and
// parse failures are kept distinct so callers can emit the same per-key log
// lines as the serial loop; the zero value is an unreadable entry.
type metaReadResult struct {
	meta     ObjectMetadata
	readErr  error
	parseErr error
}

// batchWorkerCount returns the bounded worker count for batch meta reads:
// min(batchMetaReadWorkers, runtime.NumCPU()), at least 1.
func batchWorkerCount() int {
	return min(max(runtime.NumCPU(), 1), batchMetaReadWorkers)
}

// readMetasBatch reads and parses the .meta files at the given paths with a
// bounded worker pool and returns one result per path, preserving input
// order. The batch is read-only: no locks are taken. That is safe against
// concurrent writers because every meta write goes through writeFileAtomic
// (temp file + fsync + rename in the same directory), so a reader observes
// either the old or the new file, never a torn one; per-path writer
// serialization (lockObject) is writer-side only and needs no reader side.
func readMetasBatch(paths []string, workers int) []metaReadResult {
	results := make([]metaReadResult, len(paths))
	if len(paths) == 0 {
		return results
	}
	workers = min(max(workers, 1), len(paths))

	// Index-strided work distribution: worker w handles paths w, w+workers,
	// ... writing only its own slots, so results land pre-ordered in the
	// pre-sized slice with no channel fan-in or extra allocations.
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < len(paths); i += workers {
				b, err := os.ReadFile(paths[i]) //nolint:gosec // G703: path built from validateObjectKey-checked keys joined onto the metadata dir.
				if err != nil {
					results[i] = metaReadResult{readErr: err}
					continue
				}
				var m ObjectMetadata
				if err := json.Unmarshal(b, &m); err != nil {
					results[i] = metaReadResult{parseErr: err}
					continue
				}
				results[i] = metaReadResult{meta: m}
			}
		}(w)
	}
	wg.Wait()
	return results
}
