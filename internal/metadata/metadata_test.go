package metadata

import (
	"context"
	"testing"
	"time"
)

// fakeProvider is the canonical test double; leaves 02-04 reuse the pattern.
type fakeProvider struct {
	name     string
	probeRes ProbeResult
	probeErr error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Probe(ctx context.Context, bucketPath string) (ProbeResult, error) {
	return f.probeRes, f.probeErr
}
func (f *fakeProvider) History(ctx context.Context, bucketPath, key string, q HistoryQuery) ([]ObjectEvent, error) {
	return nil, nil
}
func (f *fakeProvider) Purge(ctx context.Context, bucketPath string) error { return nil }

func TestObjectEventZeroValueIsValid(t *testing.T) {
	// The frozen struct must remain assignable/comparable with zero values
	// and its field names/positions must not drift.
	var e ObjectEvent
	if e.Op != "" || e.Txg != 0 || e.SizeOld != 0 || e.SizeNew != 0 ||
		e.UID != 0 || e.GID != 0 || !e.Timestamp.IsZero() {
		t.Fatal("zero-value ObjectEvent drifted from frozen contract")
	}
	q := HistoryQuery{MaxEvents: 10, Since: time.Unix(0, 0)}
	if q.MaxEvents != 10 || q.Since.IsZero() {
		t.Fatal("HistoryQuery drifted")
	}
	var _ MetadataProvider = (*fakeProvider)(nil) // interface shape check
}
