// Package fsbackend — list.go: the storage-facing half of ListObjectsV2,
// extracted from package main's object_handlers.go (collectObjectKeys +
// listObjectsFromKeys + appendEntries + gatherListWindow, c334f75 batched
// meta reads). Cursor/prefix/delimiter/maxKeys semantics are preserved
// exactly — including the leaf-5.1 [a]-4 merged-order rule and the V2
// token-inside-group consumption rule. encoding-type=url stays HANDLER-side
// (the backend always returns raw keys; the S3 frontend encodes).
package fsbackend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// listKeyParams is the backend-neutral projection of the cursor fields
// objectmodel.ListParams carries: ContinuationToken acts as an opaque
// marker equivalent to the conformance pin (the first key of the next
// page, listed — exclude strictly below); StartAfter excludes at-or-below.
type listKeyParams struct {
	prefix            string
	delimiter         string
	continuationToken string
	startAfter        string
	maxKeys           int
}

// listEntry is a page-eligible object entry: it survived the cursor, prefix
// and delimiter filters, but its metadata has not been read yet.
type listEntry struct {
	objectKey string
}

// listWindow is one batch-gather window: page-eligible items (keys and
// delimiter roll-ups) in merged KEY order, plus gather-loop bookkeeping.
type listWindow struct {
	entries []listEntry
	// items is the merged-order page: prefix items reference the prefix,
	// key items reference entries[entryIdx]. Preserves AWS ordering so a
	// key gathered before a roll-up fills the page first (leaf 5.1 [a]-4).
	items []listItem
	// advanced is the index into allObjectKeys just past the last key this
	// window examined.
	advanced int
}

type listItem struct {
	isPrefix bool
	prefix   string
	entryIdx int // index into window.entries when isPrefix == false
}

// batchMetaReadWorkers bounds the batched sidecar-read worker pool (the
// c334f75 design: 8-32 workers).
const batchMetaReadWorkers = 8

// batchWorkerCount returns the bounded worker count for batch meta reads:
// min(batchMetaReadWorkers, runtime.NumCPU()), at least 1.
func batchWorkerCount() int {
	return min(max(runtime.NumCPU(), 1), batchMetaReadWorkers)
}

// metaReadResult is the per-path outcome of a batched sidecar read.
// Read and parse failures are kept distinct: unreadable entries are
// SKIPPED (never counted), matching the pre-seam listing behavior.
type metaReadResult struct {
	meta     legacyMeta
	readErr  error
	parseErr error
}

// readMetasBatch reads and parses the .meta files at the given paths with a
// bounded worker pool and returns one result per path, preserving input
// order. Read-only (no locks): every sidecar write goes through
// writeFileAtomic, so a reader observes either the old or the new file,
// never a torn one.
func readMetasBatch(paths []string, workers int) []metaReadResult {
	results := make([]metaReadResult, len(paths))
	if len(paths) == 0 {
		return results
	}
	workers = min(max(workers, 1), len(paths))

	// Index-strided work distribution: pre-ordered results, no fan-in.
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < len(paths); i += workers {
				b, err := os.ReadFile(paths[i]) //nolint:gosec // G703: path built from validateKey-checked keys joined onto the metadata dir.
				if err != nil {
					results[i] = metaReadResult{readErr: err}
					continue
				}
				var m legacyMeta
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

// List enumerates a bucket's keys applying prefix, delimiter roll-up,
// cursor, and maxKeys truncation. Ordering is lexicographic by key;
// CommonPrefixes merge-sort with objects into one sequence (conformance
// pin). MaxKeys <= 0 means no truncation (implementation default).
// ContinuationToken is an opaque marker == StartAfter-equivalent per the
// conformance pin: exclude keys strictly below it.
func (f *FS) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	if err := ctx.Err(); err != nil {
		return objectmodel.ListPage{}, err
	}
	bucketPath := f.bucketPath(bucket)
	if _, err := os.Stat(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return objectmodel.ListPage{}, objectmodel.ErrNoSuchBucket(bucket)
		}
		return objectmodel.ListPage{}, backend.ToObjectModelError(err)
	}

	params := listKeyParams{
		prefix:            p.Prefix,
		delimiter:         p.Delimiter,
		continuationToken: p.ContinuationToken,
		startAfter:        p.StartAfter,
		maxKeys:           p.MaxKeys,
	}
	// Conformance pin: MaxKeys <= 0 = implementation default (no
	// truncation for small listings). The pre-seam max-keys=0 → empty
	// result is the S3 HANDLER's short-circuit (leaf-2.4 fix 13), applied
	// before the handler calls through the seam — the backend itself
	// treats an unset/non-positive budget as unbounded.
	if p.MaxKeys <= 0 {
		params.maxKeys = int(^uint(0) >> 1)
	}
	metadataDir := filepath.Join(bucketPath, metadataDirName)
	allObjectKeys, err := collectObjectKeys(metadataDir)
	if err != nil {
		return objectmodel.ListPage{}, backend.ToObjectModelError(err)
	}
	sort.Strings(allObjectKeys)
	if err := ctx.Err(); err != nil {
		return objectmodel.ListPage{}, err
	}

	truncated, nextToken, objects, commonPrefixes, _ := listObjectsFromKeys(allObjectKeys, params, metadataDir, ctx)
	if truncated && nextToken == "" {
		// Pre-seam leaf-2.4 fix 14: a truncation without a token means no
		// next page exists → report not-truncated.
		truncated = false
	}
	// Conformance pin: NextToken empty when not truncated.
	if !truncated {
		nextToken = ""
	}
	return objectmodel.ListPage{
		Objects:        objects,
		CommonPrefixes: commonPrefixes,
		IsTruncated:    truncated,
		NextToken:      nextToken,
	}, nil
}

// collectObjectKeys walks the metadata directory and returns every object
// key (sidecar path relative to metadataDir, minus the .meta suffix).
// A missing metadataDir is not an error — it is an empty bucket.
func collectObjectKeys(metadataDir string) ([]string, error) {
	var allObjectKeys []string
	err := filepath.WalkDir(metadataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".uploads" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".meta") {
			return nil
		}
		relPath, err := filepath.Rel(metadataDir, path)
		if err != nil {
			return nil //nolint:nilerr // unreachable in practice — skip malformed paths.
		}
		allObjectKeys = append(allObjectKeys, strings.TrimSuffix(relPath, ".meta"))
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return allObjectKeys, nil
	}
	return allObjectKeys, err
}

// listObjectsFromKeys walks the sorted key list applying the cursor, prefix,
// delimiter roll-up and maxKeys truncation. Returns the truncation flag,
// next token, object entries, common prefixes, and the page's last emitted
// item (kept for signature parity with the extracted code).
func listObjectsFromKeys(allObjectKeys []string, p listKeyParams, metadataDir string, ctx context.Context) (truncated bool, nextToken string, objects []objectmodel.Object, commonPrefixes []string, lastItem string) {
	processedCount := 0
	seenPrefixes := make(map[string]struct{})

	i := 0
	for i < len(allObjectKeys) {
		if err := ctx.Err(); err != nil {
			return truncated, nextToken, objects, commonPrefixes, lastItem
		}
		need := p.maxKeys - processedCount
		if p.maxKeys > 0 && need <= 0 {
			break
		}

		window := gatherListWindow(allObjectKeys, i, p, processedCount, seenPrefixes, &truncated, &nextToken)
		i = window.advanced
		if len(window.items) == 0 {
			// Nothing page-eligible left in the key space.
			break
		}

		// Batch-read the window's key metas concurrently (c334f75 Option A).
		paths := make([]string, len(window.entries))
		for j, e := range window.entries {
			paths[j] = filepath.Join(metadataDir, e.objectKey+".meta")
		}
		metas := readMetasBatch(paths, batchWorkerCount())

		// Emit items in MERGED KEY ORDER (leaf 5.1 [a]-4). Keys whose meta
		// is unreadable/unparsable are skipped (they never counted).
		metaIdx := 0
		lastEmittedItem := ""
		for _, item := range window.items {
			if p.maxKeys > 0 && processedCount >= p.maxKeys {
				// Budget exhausted mid-window. The next page resumes after
				// the LAST EMITTED item (key or prefix); for windows the
				// gather step already cut short, the B1 block below refines
				// the fallback token the same way (delimiter pages).
				truncated = true
				if lastEmittedItem != "" {
					nextToken = lastEmittedItem
				}
				break
			}
			if item.isPrefix {
				commonPrefixes = append(commonPrefixes, item.prefix)
				lastEmittedItem = item.prefix
				processedCount++
				continue
			}
			e := window.entries[item.entryIdx]
			res := metas[metaIdx]
			metaIdx++
			if res.readErr != nil || res.parseErr != nil {
				continue
			}
			objects = append(objects, objectmodel.Object{
				Key:          e.objectKey,
				Size:         res.meta.ContentLength,
				ETag:         res.meta.ETag,
				LastModified: res.meta.LastModified,
				ContentType:  res.meta.ContentType,
			})
			lastEmittedItem = e.objectKey
			processedCount++
		}
		lastItem = lastEmittedItem
		// B1 fix (bughunt finding B1): noteBudgetExhausted seeds nextToken
		// with the first unconsumed key — for a delimiter listing that key
		// can sit INSIDE a roll-up group the page never emitted, and resume
		// would then silently swallow the whole group. Delimiter pages use
		// the last EMITTED item (key or prefix) as the token, so the next
		// page resumes strictly after everything this page emitted and can
		// still roll up groups the token merely sorts before. Flat listings
		// keep the first-next-key token (pinned by TestListMaxKeysTruncation
		// + TestListMarkerExclusion).
		if p.delimiter != "" && truncated && lastEmittedItem != "" {
			nextToken = lastEmittedItem
		}
		if p.maxKeys > 0 && processedCount >= p.maxKeys {
			break
		}
	}

	return truncated, nextToken, objects, commonPrefixes, lastItem
}

// gatherListWindow collects the next window of page-eligible items
// (cursor/prefix/delimiter pre-filter — no meta I/O) in merged key order.
func gatherListWindow(allObjectKeys []string, start int, p listKeyParams, processedCount int, seenPrefixes map[string]struct{}, truncated *bool, nextToken *string) (window listWindow) {
	need := p.maxKeys - processedCount
	if p.maxKeys <= 0 {
		need = int(^uint(0) >> 1) // unbounded: implementation default
	} else if need <= 0 {
		window.advanced = start
		return window
	}

	i := start
	for i < len(allObjectKeys) {
		if p.maxKeys > 0 && len(window.items) >= need {
			// Page budget consumed in merged order.
			window.advanced = noteBudgetExhausted(allObjectKeys, i, p, seenPrefixes, truncated, nextToken)
			return window
		}
		objectKey := allObjectKeys[i]
		i++
		if keyExcludedByCursor(objectKey, p) {
			continue
		}
		if !keyMatchesPrefixFilter(objectKey, p) {
			continue
		}
		if p.delimiter != "" {
			if consumed := gatherDelimiterKey(&window, objectKey, p, seenPrefixes); consumed {
				continue
			}
		}
		window.entries = append(window.entries, listEntry{objectKey: objectKey})
		window.items = append(window.items, listItem{entryIdx: len(window.entries) - 1})
	}
	window.advanced = i
	return window
}

// gatherDelimiterKey folds one delimiter-delimited key into the window: it
// registers the key's roll-up prefix as a page item, or skips it as already
// seen/cursor-consumed/outside the request prefix (consumed=true), or
// reports consumed=false meaning the key is a plain page entry.
func gatherDelimiterKey(window *listWindow, objectKey string, p listKeyParams, seenPrefixes map[string]struct{}) (consumed bool) {
	keyPartAfterRequestPrefix := objectKey
	if strings.HasPrefix(objectKey, p.prefix) {
		keyPartAfterRequestPrefix = objectKey[len(p.prefix):]
	} else if p.prefix != "" {
		return true // outside the request prefix: excluded
	}
	idx := strings.Index(keyPartAfterRequestPrefix, p.delimiter)
	if idx == -1 {
		return false // no delimiter after the prefix: plain key
	}
	commonPrefixValue := p.prefix + keyPartAfterRequestPrefix[:idx+len(p.delimiter)]
	if _, exists := seenPrefixes[commonPrefixValue]; exists {
		return true // duplicate roll-up: free (dedupe before counting)
	}
	// Leaf 5.1 [a]-4: a cursor at or beyond the roll-up consumed the whole
	// group (V1 marker semantics; V2 token INSIDE the group likewise).
	if groupConsumedByCursor(commonPrefixValue, p) {
		return true
	}
	seenPrefixes[commonPrefixValue] = struct{}{}
	window.items = append(window.items, listItem{isPrefix: true, prefix: commonPrefixValue})
	return true
}

// groupConsumedByCursor reports whether a delimiter roll-up group was fully
// consumed by the request cursor (leaf 5.1 [a]-4): the continuation token is
// the page's last emitted item in merged order, so a token lying INSIDE or AT
// the prefix group means the page that issued the token already emitted the
// group; a token strictly BEFORE the group (e.g. plain key "a" before the
// un-emitted group "a/", bughunt finding B1) does not consume it. Otherwise
// V1-style at-or-below exclusion applies.
func groupConsumedByCursor(commonPrefixValue string, p listKeyParams) bool {
	if p.continuationToken != "" {
		return strings.HasPrefix(p.continuationToken, commonPrefixValue)
	}
	return keyExcludedByCursor(commonPrefixValue, p)
}

// noteBudgetExhausted finalizes the window once the merged-order page budget
// is consumed: truncated only if some LATER key still yields a NEW page item
// (leaf 5.1 [a]-4). The token is the first unconsumed key as a fallback;
// the emit loop refines it to the last emitted item.
func noteBudgetExhausted(allObjectKeys []string, i int, p listKeyParams, seenPrefixes map[string]struct{}, truncated *bool, nextToken *string) (advanced int) {
	hasMore := false
	for _, later := range allObjectKeys[i:] {
		if keyExcludedByCursor(later, p) || !keyMatchesPrefixFilter(later, p) {
			continue
		}
		if p.delimiter != "" {
			after := later
			if strings.HasPrefix(later, p.prefix) {
				after = later[len(p.prefix):]
			}
			if idx := strings.Index(after, p.delimiter); idx != -1 {
				pv := p.prefix + after[:idx+len(p.delimiter)]
				if _, seen := seenPrefixes[pv]; seen {
					continue
				}
				if groupConsumedByCursor(pv, p) {
					continue
				}
				hasMore = true
				break
			}
		}
		hasMore = true // plain key → new item
		break
	}
	if hasMore {
		*truncated = true
		if i < len(allObjectKeys) {
			*nextToken = allObjectKeys[i]
		}
	}
	return i
}

// keyExcludedByCursor reports whether objectKey is excluded by the cursor.
//   - continuation-token, flat listing (no delimiter): the token IS the first
//     key of the next page, so the boundary key must be LISTED — exclude
//     strictly below it (<).
//   - continuation-token, delimiter listing: the token is the last EMITTED
//     item in merged order (key or roll-up prefix, B1 fix), so resume
//     excludes everything at or below it (<=); the next page must re-derive
//     un-emitted roll-up groups (groupConsumedByCursor re-emits groups the
//     token merely sorts before).
//   - start-after: exclusive marker — exclude everything at or below it (<=).
func keyExcludedByCursor(objectKey string, p listKeyParams) bool {
	if p.continuationToken != "" {
		if p.delimiter != "" {
			return objectKey <= p.continuationToken
		}
		return objectKey < p.continuationToken
	}
	if p.startAfter != "" {
		return objectKey <= p.startAfter
	}
	return false
}

// keyMatchesPrefixFilter reports whether objectKey passes the prefix filter.
func keyMatchesPrefixFilter(objectKey string, p listKeyParams) bool {
	return p.prefix == "" || strings.HasPrefix(objectKey, p.prefix)
}
