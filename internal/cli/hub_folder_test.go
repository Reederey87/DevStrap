package cli

import (
	"testing"

	"github.com/spf13/viper"
)

func TestParseFolderURI(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		wantPath string
		wantErr  bool
	}{
		{name: "absolute path", uri: "folder:/Users/me/Dropbox/devstrap-hub", wantPath: "/Users/me/Dropbox/devstrap-hub"},
		{name: "absolute root", uri: "folder:/hub", wantPath: "/hub"},
		{name: "relative path", uri: "folder:rel/path", wantErr: true},
		{name: "empty path", uri: "folder:", wantErr: true},
		{name: "query params rejected", uri: "folder:/hub?branch=x", wantErr: true},
		{name: "not a folder uri", uri: "r2://bucket", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFolderURI(c.uri)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseFolderURI(%q) = nil error, want error", c.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFolderURI(%q) = %v, want nil", c.uri, err)
			}
			if got != c.wantPath {
				t.Fatalf("parseFolderURI(%q) = %q, want %q", c.uri, got, c.wantPath)
			}
		})
	}
}

func TestHubConfiguredFolderURI(t *testing.T) {
	cases := []struct {
		name    string
		hub     string
		wantErr bool
	}{
		{"valid absolute folder", "folder:/Users/me/Dropbox/devstrap-hub", false},
		{"relative folder", "folder:rel/path", true},
		{"empty folder path", "folder:", true},
		{"folder with query", "folder:/hub?x=1", true},
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
