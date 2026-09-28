package backend

import (
	"errors"
	"io/fs"
	"testing"

	"mini-s3/internal/objectmodel"
)

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // objectmodel.Code* constant; "" = no code expected
	}{
		{"notExist", fs.ErrNotExist, objectmodel.CodeNoSuchKey},
		{"notExistWrapped", wrapped(fs.ErrNotExist), objectmodel.CodeNoSuchKey},
		{"notSupported", ErrNotSupported, ""},
		{"unknownBackend", ErrUnknownBackend, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToObjectModelError(tc.err)
			if tc.want == "" {
				return // sentinel/nil passthrough: identity preserved, no mapping
			}
			omErr := &objectmodel.Error{}
			if !errors.As(got, &omErr) {
				t.Fatalf("ToObjectModelError(%v) = %T, want chain carrying *objectmodel.Error", tc.err, got)
			}
			if omErr.Code != tc.want {
				t.Fatalf("want code %v, got %v", tc.want, omErr.Code)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("cause %v no longer reachable via errors.Is through %v", tc.err, got)
			}
		})
	}
}

// wrapped builds a non-nil chain wrapping cause, to prove %w reachability.
func wrapped(cause error) error {
	return &chainErr{cause}
}

type chainErr struct{ cause error }

func (e *chainErr) Error() string { return "wrapped: " + e.cause.Error() }
func (e *chainErr) Unwrap() error { return e.cause }

func TestSentinels(t *testing.T) {
	if !errors.Is(ErrNotSupported, ErrNotSupported) {
		t.Fatal("sentinel identity broken: ErrNotSupported")
	}
	if !errors.Is(ErrUnknownBackend, ErrUnknownBackend) {
		t.Fatal("sentinel identity broken: ErrUnknownBackend")
	}
}

func TestToModelPassthrough(t *testing.T) {
	orig := objectmodel.ErrNoSuchKey("k")
	got := ToObjectModelError(orig)
	if !errors.Is(got, orig) {
		t.Fatalf("objectmodel.Error input must pass through: got %v", got)
	}
}
