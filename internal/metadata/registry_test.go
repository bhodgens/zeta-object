package metadata

import (
	"context"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	p := &fakeProvider{name: "test-a"}
	Register(p)
	if got := Lookup("test-a"); got != p {
		t.Fatalf("Lookup returned %v, want %v", got, p)
	}
	if got := Lookup("missing"); got != nil {
		t.Fatalf("Lookup of unknown name = %v, want nil", got)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register(&fakeProvider{name: "dup-a"})
	Register(&fakeProvider{name: "dup-a"})
}

func TestProbeAndAttach(t *testing.T) {
	Register(&fakeProvider{name: "hit", probeRes: ProbeResult{Available: true, Dataset: "tank/data"}})
	Register(&fakeProvider{name: "miss", probeRes: ProbeResult{Available: false, Reason: "not zfs"}})
	Register(&fakeProvider{name: "err", probeErr: context.DeadlineExceeded})

	got := ProbeAndAttach(context.Background(), t.TempDir())
	if len(got) != 1 || got[0] != "hit" {
		t.Fatalf("ProbeAndAttach = %v, want [hit]", got)
	}
}
