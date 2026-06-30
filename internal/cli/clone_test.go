package cli

import "testing"

// TestDeriveClonePath (PROD-01): the namespace path is derived from the
// canonical remote key as work/<org>/<repo>, stripping the host so SSH and
// HTTPS forms of the same repo derive the same path.
func TestDeriveClonePath(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"git@github.com:acme/api.git", "work/acme/api"},
		{"https://github.com/acme/api.git", "work/acme/api"},
		{"ssh://git@github.com/acme/api.git", "work/acme/api"},
		{"git@github.com:acme/org/repo.git", "work/acme/org/repo"},
		{"https://gitlab.com/sub/team/proj.git", "work/sub/team/proj"},
	}
	for _, c := range cases {
		got, err := deriveClonePath(c.remote)
		if err != nil {
			t.Fatalf("deriveClonePath(%q): %v", c.remote, err)
		}
		if got != c.want {
			t.Errorf("deriveClonePath(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
	// An invalid remote is rejected.
	if _, err := deriveClonePath("not a url"); err == nil {
		t.Fatal("expected error for invalid remote")
	}
}
