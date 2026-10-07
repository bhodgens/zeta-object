package sync

import "context"

// ChangeFeed is the event-cursor seam (leaf 05 implements it). Leaf 04's
// engine runs feed.Delta(ctx) first; when the returned fullScan is true
// it ignores changedPaths and walks the whole tree (the PROPFIND scan),
// otherwise it visits only the changed paths (plus their parent
// directories) with the same per-path diff. The default implementation,
// FullScanFeed, ALWAYS answers fullScan: leaf 04 ships scan-only
// (master decision 7: the ETag-diff scan is the universal path; no
// zeta-cache feature may REQUIRE zfs-metadata).
//
// Contract for leaf 05's cursor implementation:
//   - changedPaths are bucket-root-relative FILE keys, no leading or
//     trailing slash (the same key convention as transport.Transport).
//     Deletions are reported as the deleted key (the engine diffs the
//     key either way: an absent remote row + index row = remote-deleted).
//   - A cursor gap / provider 503 / recordsLost / ringSwap MUST be
//     answered as (paths nil, fullScan=true, nil err): the engine treats
//     fullScan as authoritative and never merges a partial path list
//     with a full walk.
//   - Delta must be safe to call before every SyncOnce and must keep its
//     cursor state in the index meta table under its own keys (leaf 05
//     owns cursor keys; the engine never reads them).
type ChangeFeed interface {
	Delta(ctx context.Context) (changedPaths []string, fullScan bool, err error)
}

// FullScanFeed is leaf 04's default ChangeFeed: every delta is a full
// scan. An Engine constructed with a nil feed uses this.
type FullScanFeed struct{}

// Delta always reports a full scan with no pre-known changed paths.
func (FullScanFeed) Delta(context.Context) ([]string, bool, error) {
	return nil, true, nil
}

var _ ChangeFeed = FullScanFeed{}
