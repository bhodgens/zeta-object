// batch_context_test.go — the batchops core hands the CALLER's context
// to the Executor verbatim (bughunt 2026-10-05 M3): a client that
// disconnects mid-batch cancels the items still to come. The core must
// never substitute a fresh context.Background() — that substitution is
// exactly what made the s3 executor's injected context dead.
package batchops

import (
	"context"
	"testing"
)

// ctxRecordingExec records the context each item call received.
type ctxRecordingExec struct {
	seen []context.Context
}

func (e *ctxRecordingExec) record(ctx context.Context) error {
	e.seen = append(e.seen, ctx)
	return nil
}

func (e *ctxRecordingExec) Copy(ctx context.Context, _, _, _ string) error { return e.record(ctx) }
func (e *ctxRecordingExec) Move(ctx context.Context, _, _, _ string) error { return e.record(ctx) }
func (e *ctxRecordingExec) Delete(ctx context.Context, _, _ string) error {
	return e.record(ctx)
}

// TestRun_PassesCallerContextToEveryItem: every item's op call receives
// the context Run was handed (a ctx value proves identity; a cancellation
// proves it is live, not a fresh Background).
func TestRun_PassesCallerContextToEveryItem(t *testing.T) {
	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "batch-scoped")

	e := &ctxRecordingExec{}
	r := &Runner{Exec: e, ValidateKey: DefaultKeyValidator}
	r.Run(base, manifest3())

	if len(e.seen) != 3 {
		t.Fatalf("executor saw %d calls, want 3", len(e.seen))
	}
	for i, ctx := range e.seen {
		if ctx == nil {
			t.Fatalf("item %d received a nil context", i)
		}
		if got := ctx.Value(ctxKey{}); got != "batch-scoped" {
			t.Errorf("item %d context value = %v, want the caller's context (the core must not substitute Background)", i, got)
		}
		if ctx.Err() != nil {
			t.Errorf("item %d context is already cancelled for a live caller", i)
		}
	}
}

// TestRun_CancelledCallerContextReachesTheExecutor: a disconnected
// client's cancelled context flows into the op calls, so an executor that
// honors it can abort the remaining items instead of running the whole
// manifest on a background context.
func TestRun_CancelledCallerContextReachesTheExecutor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e := &ctxRecordingExec{}
	r := &Runner{Exec: e, ValidateKey: DefaultKeyValidator}
	r.Run(ctx, manifest3())

	if len(e.seen) != 3 {
		t.Fatalf("executor saw %d calls, want 3", len(e.seen))
	}
	for i, got := range e.seen {
		if got == nil {
			t.Fatalf("item %d received a nil context", i)
		}
		if got.Err() == nil {
			t.Errorf("item %d received a live context — the caller's cancellation never reached the executor", i)
		}
	}
}
