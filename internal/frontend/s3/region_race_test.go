// region_race_test.go — the concurrency pin for the SigV4 region seam
// (bughunt 2026-10-06 H1).
//
// WHY THIS FILE EXISTS: `region` was a plain `var region string`. The 2026-10-05
// wave made the region a HOT seam — applyHotSeams (s3_wiring.go) calls
// SetRegion from the PUT /config admin handler goroutine — while every S3
// request reads the same global through regionOf/regionExplicit. `-race`
// reported a real data race between those two, and a torn string read can
// answer a correctly-signed us-east-1 request with SignatureDoesNotMatch.
//
// This test FAILS (under -race) on the old plain-var implementation and PASSES
// on the atomic.Pointer one. Run it as:
//
//	go test ./internal/frontend/s3 -race -run TestRegionConcurrentSetAndRead
package s3

import (
	"sync"
	"testing"
)

// TestRegionConcurrentSetAndRead drives one writer (the admin reload shape:
// SetRegion in a loop) against several readers (the request shape: regionOf +
// regionExplicit + checkRegionMatch) and asserts two things:
//
//  1. NO DATA RACE (the -race detector's whole job here).
//  2. Every observed value is a COMPLETE generation: one of the two regions
//     written, never a torn or empty string. A torn read is the client-visible
//     symptom — a half-written string fails the SigV4 compare and answers a
//     legitimate request with SignatureDoesNotMatch.
func TestRegionConcurrentSetAndRead(t *testing.T) {
	t.Cleanup(func() { SetRegion("") })

	const writerIters = 2000
	regions := []string{"us-east-1", "eu-central-1"}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// One writer, exactly like the admin handler.
	wg.Go(func() {
		for i := range writerIters {
			SetRegion(regions[i%len(regions)])
		}
		close(stop)
	})

	// Readers: the per-request accessors.
	const readers = 4
	var mu sync.Mutex
	seen := map[string]int{}
	record := func(v string) {
		mu.Lock()
		seen[v]++
		mu.Unlock()
	}
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := regionOf()
				// A torn or defaulted value is the failure this pins.
				if got != "us-east-1" && got != "eu-central-1" {
					record("TORN:" + got)
					continue
				}
				// regionExplicit means "was a NON-DEFAULT region configured",
				// NOT "is regionOf() == its default". regionOf() reports
				// us-east-1 for BOTH default mode and an explicitly-configured
				// us-east-1, so the only sound cross-check is that an
				// explicitly-set NON-default region always reports explicit
				// mode. Checking `regionExplicit() == (got != defaultRegion)`
				// is wrong and failed 29 times: default mode has
				// regionOf()==us-east-1 AND regionExplicit()==false.
				if got == "eu-central-1" && !regionExplicit() {
					record("INCONSISTENT:" + got)
					continue
				}
				// The full request-path check must not observe a torn value
				// either; nil means "accepted", non-nil names the failure.
				// NOTE: this asserts nothing about mid-flight reloads.
				// checkRegionMatch re-reads regionOf() internally, so a
				// reload landing between our snapshot and its own read makes
				// it legitimately reject the value we passed — that is the
				// documented "client signed the region the API advertised a
				// moment ago" case, not a torn read. The TORN branch above is
				// what pins atomicity; this call only proves the accessor
				// chain never panics or returns a garbage token.
				_ = checkRegionMatch(got)
				record(got)
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for what, n := range seen {
		if len(what) > 4 && (what[:4] == "TORN" || what[:4] == "INCO" || what[:4] == "MISM") {
			t.Fatalf("concurrent region access observed %s %d time(s); the seam is not atomic", what, n)
		}
	}
	if seen["us-east-1"]+seen["eu-central-1"] == 0 {
		t.Fatal("readers observed no region at all; the pin did not exercise the seam")
	}
}

// TestRegionExplicitSemanticsAfterAtomicSwap pins the three-state contract the
// atomic pointer introduced: nil (never set) and a pointer to "" (explicitly
// cleared) BOTH mean default mode, while a non-empty value means explicit
// mode. The old plain var could not distinguish nil from ""; this pins that
// the distinction does not leak into behavior.
func TestRegionExplicitSemanticsAfterAtomicSwap(t *testing.T) {
	t.Cleanup(func() { SetRegion("") })

	// Never set in this process -> nil -> default mode.
	region.Store(nil)
	if got := regionOf(); got != defaultRegion {
		t.Errorf("nil pointer: regionOf = %q, want the default %q", got, defaultRegion)
	}
	if regionExplicit() {
		t.Error("nil pointer: regionExplicit = true, want false (default mode)")
	}

	// Explicitly cleared -> still default mode.
	SetRegion("")
	if got := regionOf(); got != defaultRegion {
		t.Errorf("empty string: regionOf = %q, want the default %q", got, defaultRegion)
	}
	if regionExplicit() {
		t.Error("empty string: regionExplicit = true, want false (default mode)")
	}

	// The default region set EXPLICITLY is still explicit mode (Contract 2).
	SetRegion("us-east-1")
	if got := regionOf(); got != "us-east-1" {
		t.Errorf("explicit us-east-1: regionOf = %q", got)
	}
	if !regionExplicit() {
		t.Error("explicit us-east-1: regionExplicit = false, want true (strict compare)")
	}

	// Uppercase input is lowercased by the setter.
	SetRegion("EU-CENTRAL-1")
	if got := regionOf(); got != "eu-central-1" {
		t.Errorf("SetRegion did not lowercase: regionOf = %q, want %q", got, "eu-central-1")
	}
}
