// copymove_conflict_test.go — the bughunt 2026-10-05 M2 pin: MOVE's source
// delete answers 409 where DELETE answers 409 for the SAME error class.
//
// The defect: copymove.go's MOVE tail called writeDavErrorFrom on the
// moveSourceDelete error, and davStatus has no case for the versioning
// sentinels (ErrDeleteMarkersUnsupported / ErrSnapshotsReadOnly), so they fell
// to the 500 default. delete.go branches on webdavVersioningConflict and
// answers 409 for the identical store state; the s3 CopyObject+DeleteObject
// counterpart answers 409 too. A MOVE on a snapshots-mode bucket returned 500
// — a server fault — for what every sibling surface reports as a conflict.
//
// The env here is newVerParityEnv (the same env every versioning pin in this
// package uses), so the parity assertions below inherit the production wiring
// the package's H1 pins established.
package webdav

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

func TestWebdavMOVE_SnapshotsModeSourceDeleteConflict(t *testing.T) {
	e := newVerParityEnv(t, "parity-snap-move-bucket", "snapshots")
	e.enable()

	e.davPut("snap.txt", "v1")

	// MOVE away from snap.txt: the copy half succeeds, the source delete
	// hits the snapshots store's refusal — the 500-vs-409 defect site.
	code := e.davMoveStatus("snap.txt", "moved.txt")
	if code != http.StatusConflict {
		t.Fatalf("snapshots-mode webdav MOVE = %d, want 409 (delete.go's DELETE answers 409 for the same store state; s3 CopyObject+DeleteObject too)", code)
	}

	// The refused source delete left the source intact: no marker was
	// written and no bytes were destroyed — a conflict is a refusal, not a
	// half-applied rename.
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "snap.txt")); err != nil || string(raw) != "v1" {
		t.Fatalf("source after refused MOVE = %q err=%v, want the source bytes intact", raw, err)
	}
}

func TestWebdavMOVE_NormalModeStillSucceeds(t *testing.T) {
	e := newVerParityEnv(t, "parity-sidecar-move-bucket", "sidecar")
	e.enable()

	e.davPut("snap.txt", "v1")

	// The guard against fixing the 409 by refusing MOVE outright: a normal
	// (sidecar-mode) MOVE still renames with 201.
	code := e.davMoveStatus("snap.txt", "moved.txt")
	if code != http.StatusCreated {
		t.Fatalf("sidecar-mode webdav MOVE = %d, want 201 (the conflict branch must refuse only the conflict sentinels)", code)
	}
	if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "moved.txt")); err != nil || string(raw) != "v1" {
		t.Fatalf("destination after MOVE = %q err=%v, want the moved bytes", raw, err)
	}
	// On a versioning-Enabled bucket the source delete records a MARKER and
	// deliberately suppresses the plain delete (delete.go's contract), so
	// the source data file survives behind the marker — the same state a
	// dedicated DELETE leaves. Assert the marker directly through the
	// read-side predicate both GET surfaces consult.
	hidden, err := s3.PlainObjectDeleteMarker404(e.bucketPath, e.bucket, "snap.txt")
	if err != nil {
		t.Fatalf("marker consult: %v", err)
	}
	if !hidden {
		t.Fatal("the MOVE source delete recorded no delete marker (a versioned MOVE must leave the same store state DELETE does)")
	}
}
