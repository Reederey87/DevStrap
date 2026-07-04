package cli

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestParseGitCarrierURI(t *testing.T) {
	cases := []struct {
		name       string
		uri        string
		wantRemote string
		wantBranch string
	}{
		{
			name:       "git ssh default branch",
			uri:        "git+ssh://git@host/path.git",
			wantRemote: "ssh://git@host/path.git",
			wantBranch: "main",
		},
		{
			name:       "git ssh custom branch",
			uri:        "git+ssh://git@host/path.git?branch=work",
			wantRemote: "ssh://git@host/path.git",
			wantBranch: "work",
		},
		{
			name:       "git https default branch",
			uri:        "git+https://host/path.git",
			wantRemote: "https://host/path.git",
			wantBranch: "main",
		},
		{
			name:       "git https custom branch",
			uri:        "git+https://host/path.git?branch=work",
			wantRemote: "https://host/path.git",
			wantBranch: "work",
		},
		{
			name:       "git file default branch",
			uri:        "git+file:///abs/path",
			wantRemote: "file:///abs/path",
			wantBranch: "main",
		},
		{
			name:       "git file custom branch",
			uri:        "git+file:///abs/path?branch=work",
			wantRemote: "file:///abs/path",
			wantBranch: "work",
		},
		{
			name:       "scp like default branch",
			uri:        "git@host:path.git",
			wantRemote: "git@host:path.git",
			wantBranch: "main",
		},
		{
			name:       "scp like custom branch",
			uri:        "git@host:path.git?branch=work",
			wantRemote: "git@host:path.git",
			wantBranch: "work",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotRemote, gotBranch, err := parseGitCarrierURI(c.uri)
			if err != nil {
				t.Fatalf("parseGitCarrierURI(%q) = %v, want nil", c.uri, err)
			}
			if gotRemote != c.wantRemote || gotBranch != c.wantBranch {
				t.Fatalf("parseGitCarrierURI(%q) = (%q, %q), want (%q, %q)", c.uri, gotRemote, gotBranch, c.wantRemote, c.wantBranch)
			}
		})
	}
}

func TestParseGitCarrierURIRejectsInvalid(t *testing.T) {
	const secret = "supersecret"
	cases := []struct {
		name    string
		uri     string
		notWant string
	}{
		{
			name:    "embedded password",
			uri:     "git+https://user:" + secret + "@host/path.git",
			notWant: secret,
		},
		{
			name: "empty",
			uri:  "",
		},
		{
			name: "unknown scheme",
			uri:  "git+ftp://host/path.git",
		},
		{
			name: "invalid branch",
			uri:  "git+ssh://git@host/path.git?branch=-bad",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := parseGitCarrierURI(c.uri)
			if err == nil {
				t.Fatalf("parseGitCarrierURI(%q) = nil error, want error", c.uri)
			}
			if c.notWant != "" && strings.Contains(err.Error(), c.notWant) {
				t.Fatalf("parseGitCarrierURI(%q) error leaks %q: %q", c.uri, c.notWant, err.Error())
			}
		})
	}
}

func TestHubConfiguredGitCarrierURI(t *testing.T) {
	cases := []struct {
		name    string
		hub     string
		wantErr bool
	}{
		{"valid git ssh", "git+ssh://git@host/path.git", false},
		{"invalid git scheme", "git+ftp://host/path.git", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := &options{v: viper.New()}
			opts.v.Set("hub", c.hub)
			err := hubConfigured(opts, "")
			if c.wantErr && err == nil {
				t.Fatalf("hubConfigured(%q) = nil error, want error", c.hub)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("hubConfigured(%q) = %v, want nil", c.hub, err)
			}
		})
	}
}
