package backend

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stubCtor is a constructor that never gets called by these tests; the
// registry only stores constructors, it does not invoke them.
func stubCtor(BackendConfig) (Backend, error) { return nil, errors.New("not constructed in test") }

func TestRegistryRegisterLookup(t *testing.T) {
	cases := []struct {
		name    string
		wantErr error
	}{
		{"testfs", nil},
		{"nope", ErrUnknownBackend},
	}
	Register("testfs", stubCtor)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := Lookup(tc.name)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || fn == nil {
				t.Fatalf("want ctor, got %v", err)
			}
		})
	}
}

// TestLookupErrorMessageNamesRegistrations pins leaf 03's diagnostics
// convention: the unknown-name error carries the sorted registered names so
// startup misconfigurations are self-explaining.
func TestLookupErrorMessageNamesRegistrations(t *testing.T) {
	prefix := fmt.Sprintf("errmsg%d", time.Now().UnixNano())
	Register(prefix+"-alpha", stubCtor)
	Register(prefix+"-beta", stubCtor)

	_, err := Lookup(prefix + "-missing")
	if err == nil {
		t.Fatal("want error for unknown backend name")
	}
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("want ErrUnknownBackend identity, got %v", err)
	}
	if !strings.Contains(err.Error(), prefix+"-alpha") ||
		!strings.Contains(err.Error(), prefix+"-beta") {
		t.Fatalf("error message should list registered names, got: %v", err)
	}
	// Sorted within the unique prefix.
	ia := strings.Index(err.Error(), prefix+"-alpha")
	ib := strings.Index(err.Error(), prefix+"-beta")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("registered names not sorted in message: %v", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate registration")
		}
	}()
	Register("dupe", stubCtor)
	Register("dupe", stubCtor)
}

func TestNamesSorted(t *testing.T) {
	prefix := fmt.Sprintf("namesort%d", time.Now().UnixNano())
	Register(prefix+"-b", stubCtor)
	Register(prefix+"-a", stubCtor)

	var got []string
	for _, n := range Names() {
		if strings.HasPrefix(n, prefix) {
			got = append(got, n)
		}
	}
	want := []string{prefix + "-a", prefix + "-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() filtered to prefix = %v, want %v", got, want)
	}
}
