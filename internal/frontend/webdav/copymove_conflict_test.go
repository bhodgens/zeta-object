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
	"net/http/httptest"
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

// TestWebdavMOVE_SelfDestinationIs403 (bughunt H9, RFC 4918 §9.9.4): a MOVE
// whose Destination IS the request URI must be refused with 403 BEFORE any
// backend call. Without the guard the flow Puts the source bytes back onto
// the same key and then runs the source delete — on a versioning-Enabled
// bucket that delete records a marker while the wire answered 204, so the
// plain object vanishes: a self-MOVE destroyed the object.
func TestWebdavMOVE_SelfDestinationIs403(t *testing.T) {
	for _, mode := range []string{"sidecar", "snapshots"} {
		t.Run(mode, func(t *testing.T) {
			e := newVerParityEnv(t, "self-move-"+mode+"-bkt", mode)
			e.enable()

			e.davPut("self.txt", "keep me")

			code := e.davMoveStatus("self.txt", "self.txt")
			if code != http.StatusForbidden {
				t.Fatalf("%s-mode self-MOVE = %d, want 403 (RFC 4918 9.9.4)", mode, code)
			}

			// The object must be UNTOUCHED: the bytes survive the
			// refused MOVE.
			if raw, err := os.ReadFile(filepath.Join(e.bucketPath, "self.txt")); err != nil || string(raw) != "keep me" {
				t.Fatalf("self-MOVE clobbered the object: %q err=%v", raw, err)
			}
			// The core of the defect: no delete marker recorded, so
			// every plain view still serves the object. (The marker
			// consult runs in sidecar mode only — a snapshots-store
			// List in this env has no tracked zfs dataset to resolve,
			// the same env limit TestWebdavPUT_SnapshotsModeParity
			// documents; the 403 + intact bytes above carry the
			// snapshots arm.)
			if mode == "sidecar" {
				hidden, err := s3.PlainObjectDeleteMarker404(e.bucketPath, e.bucket, "self.txt")
				if err != nil {
					t.Fatalf("marker consult: %v", err)
				}
				if hidden {
					t.Fatal("self-MOVE recorded a delete marker (the guard must refuse before the source delete)")
				}
			}
		})
	}
}

// TestWebdavMOVE_SelfDestinationAcrossModes pins the guard's comparison
// rules on an unversioned (stub-backend, mode A) mount: the same bucket+key
// is refused whether the Destination arrives as a bare path or a full URL,
// and a same-bucket DIFFERENT key still moves (the guard must not overfire).
func TestWebdavMOVE_SelfDestinationAcrossModes(t *testing.T) {
	f, be := newTestFrontend(Config{})
	be.seed("photos", "a.txt", []byte("x"))

	move := func(dest string) int {
		req := httptest.NewRequest("MOVE", "/photos/a.txt", nil)
		req.Header.Set("Destination", dest)
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := move("/photos/a.txt"); got != http.StatusForbidden {
		t.Fatalf("self-MOVE bare path = %d, want 403", got)
	}
	if got := move("https://example.test/photos/a.txt"); got != http.StatusForbidden {
		t.Fatalf("self-MOVE full URL = %d, want 403", got)
	}
	if be.hasKey("photos", "a.txt") == false {
		t.Fatal("self-MOVE deleted the object")
	}
	// A different key in the same bucket is a legal MOVE (the guard must
	// not refuse renames).
	if got := move("/photos/b.txt"); got != http.StatusCreated {
		t.Fatalf("normal MOVE after guard = %d, want 201 (guard overfired)", got)
	}
	if !be.hasKey("photos", "b.txt") || be.hasKey("photos", "a.txt") {
		t.Fatal("normal MOVE did not rename")
	}
}
