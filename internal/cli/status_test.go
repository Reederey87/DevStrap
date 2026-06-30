package cli

import "testing"

// P5-PROD-01: deriveDisplayStatus must branch only on states writers actually
// produce, and the headline "ready" must be reachable (materialized + clean).
func TestDeriveDisplayStatus(t *testing.T) {
	cases := []struct {
		materialization string
		dirty           string
		want            string
	}{
		{"skeleton", "unknown", "skeleton"},
		{"failed", "unknown", "failed"},
		{"materialized-empty", "clean", "empty checkout"},
		{"available", "clean", "ready"}, // the headline readiness state
		{"available", "dirty", "dirty"},
		{"available", "diverged", "dirty"},
		{"available", "unknown", "available"},
		{"available", "", "available"},
	}
	for _, c := range cases {
		if got := deriveDisplayStatus(c.materialization, c.dirty); got != c.want {
			t.Errorf("deriveDisplayStatus(%q,%q) = %q, want %q", c.materialization, c.dirty, got, c.want)
		}
	}
}
