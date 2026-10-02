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

// TestRegionExplicit pins the strict/permissive mode flag (region-config-
// 2026-10 leaf 02): regionExplicit() is false in default mode (SetRegion
// never called with a value, or called with "") and true after any
// SetRegion with a non-empty value - including "us-east-1" itself, because
// an explicit config value means STRICT compare (Contract 2).
func TestRegionExplicit(t *testing.T) {
	t.Run("default mode is not explicit", func(t *testing.T) {
		SetRegion("") // reset to the default
		if regionExplicit() {
			t.Fatal("regionExplicit() = true in default mode, want false")
		}
	})

	t.Run("SetRegion with a value is explicit", func(t *testing.T) {
		defer SetRegion("")
		SetRegion("eu-west-1")
		if !regionExplicit() {
			t.Fatal("regionExplicit() = false after SetRegion(\"eu-west-1\"), want true")
		}
	})

	t.Run("SetRegion with the default value is still explicit", func(t *testing.T) {
		defer SetRegion("")
		SetRegion("us-east-1")
		if !regionExplicit() {
			t.Fatal("regionExplicit() = false after explicit SetRegion(\"us-east-1\"), want true (explicit config value = strict)")
		}
	})

	t.Run("SetRegion empty restores permissive mode", func(t *testing.T) {
		defer SetRegion("")
		SetRegion("eu-west-1")
		SetRegion("")
		if regionExplicit() {
			t.Fatal("regionExplicit() = true after SetRegion(\"\"), want false")
		}
	})
}
