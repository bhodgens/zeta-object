package s3

import "testing"

// Region accessor tests (region-config-2026-10 leaf 01, Contract 1).
// SetRegion is startup-only config wiring: the tests run sequentially and
// each resets the state it changes.
func TestRegionOf(t *testing.T) {
	t.Run("default without SetRegion is us-east-1", func(t *testing.T) {
		SetRegion("us-east-1") // normalize state from any earlier test
		SetRegion("")          // reset to the default
		if got := regionOf(); got != "us-east-1" {
			t.Fatalf("regionOf() = %q, want default %q", got, "us-east-1")
		}
	})

	t.Run("SetRegion sets the region", func(t *testing.T) {
		defer SetRegion("")
		SetRegion("eu-west-1")
		if got := regionOf(); got != "eu-west-1" {
			t.Fatalf("regionOf() = %q, want %q", got, "eu-west-1")
		}
	})

	t.Run("SetRegion empty restores the default", func(t *testing.T) {
		SetRegion("eu-west-1")
		SetRegion("")
		if got := regionOf(); got != "us-east-1" {
			t.Fatalf("regionOf() = %q, want default %q after SetRegion(\"\")", got, "us-east-1")
		}
	})

	t.Run("SetRegion lowercases", func(t *testing.T) {
		defer SetRegion("")
		SetRegion("EU-WEST-1")
		if got := regionOf(); got != "eu-west-1" {
			t.Fatalf("regionOf() = %q, want lowercased %q", got, "eu-west-1")
		}
	})
}
