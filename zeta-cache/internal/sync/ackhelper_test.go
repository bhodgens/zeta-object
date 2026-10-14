package sync

import "context"

// deltaThenAck drives Delta and, when the feed kept a page pending
// (fullScan=false AND first-contact full scans), simulates the engine's
// post-apply Ack - the engine acks after EVERY successful pass, full or
// delta. Returns (paths, fullScan, err) exactly like Delta.
func deltaThenAck(ctx context.Context, f *CursorFeed) ([]string, bool, error) {
	paths, full, err := f.Delta(ctx)
	if err == nil && f.pendValid {
		f.Ack(ctx)
	}
	return paths, full, err
}
