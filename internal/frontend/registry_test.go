package frontend_test

import (
	"net/http"
	"testing"

	"mini-s3/internal/auth"
	"mini-s3/internal/frontend"
)

func regFake(name string) frontend.Frontend { return namedFake{name: name} }

type namedFake struct{ name string }

func (f namedFake) Name() string                      { return f.name }
func (namedFake) Handler() http.Handler               { return http.NotFoundHandler() }
func (namedFake) Authenticator() auth.Authenticator   { return nil }
func (namedFake) Capabilities() frontend.ProtocolCaps { return frontend.ProtocolCaps{} }

func TestRegistry_RegisterLookupAll(t *testing.T) {
	tests := []struct {
		name      string
		register  []string // in this order
		lookup    string
		wantFound bool
		wantAll   []string // expected All() order
	}{
		{"lookup hit", []string{"zeta", "alpha"}, "alpha", true, []string{"alpha", "zeta"}},
		{"lookup miss", []string{"zeta"}, "nope", false, []string{"zeta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := frontend.NewRegistry()
			for _, n := range tt.register {
				if err := r.Register(regFake(n)); err != nil {
					t.Fatalf("Register(%q): %v", n, err)
				}
			}
			_, found := r.Lookup(tt.lookup)
			if found != tt.wantFound {
				t.Fatalf("Lookup(%q) found = %v, want %v", tt.lookup, found, tt.wantFound)
			}
			var got []string
			for _, f := range r.All() {
				got = append(got, f.Name())
			}
			if len(got) != len(tt.wantAll) {
				t.Fatalf("All() = %v, want %v", got, tt.wantAll)
			}
			for i := range got {
				if got[i] != tt.wantAll[i] {
					t.Fatalf("All() = %v, want sorted %v", got, tt.wantAll)
				}
			}
		})
	}
}

func TestRegistry_DuplicateAndInvalid(t *testing.T) {
	tests := []struct {
		name      string
		first     string
		second    string
		wantError bool
	}{
		{"duplicate name rejected", "s3", "s3", true},
		{"empty name rejected", "valid", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := frontend.NewRegistry()
			// An empty first name is itself the invalid input being tested;
			// its error is expected, not fatal.
			if err := r.Register(regFake(tt.first)); err != nil && tt.first != "" {
				t.Fatalf("first Register(%q): %v", tt.first, err)
			}
			err := r.Register(regFake(tt.second))
			if (err != nil) != tt.wantError {
				t.Fatalf("second Register(%q) err = %v, wantError %v", tt.second, err, tt.wantError)
			}
		})
	}
}
