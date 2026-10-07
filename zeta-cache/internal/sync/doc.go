// Package sync owns the two-way sync engine: the ETag-diff PROPFIND scan
// (leaf 04), the event-cursor optimization path (leaf 05, conditional on
// zeta-object#15), and the upload path with If-Match. Leaf 01 ships only
// this placeholder; the daemon runs an idle loop until those leaves land.
package sync
