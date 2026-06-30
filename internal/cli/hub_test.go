package cli

import (
	"testing"

	"github.com/spf13/viper"
)

func TestParseHubURI(t *testing.T) {
	cases := []struct {
		uri      string
		wantSpec hubSpec
		wantErr  bool
	}{
		{"r2://devstrap-test", hubSpec{scheme: "r2", bucket: "devstrap-test"}, false},
		{"s3://my-bucket", hubSpec{scheme: "s3", bucket: "my-bucket"}, false},
		{"r2://devstrap-test?endpoint=http://localhost:9000", hubSpec{scheme: "r2", bucket: "devstrap-test", endpoint: "http://localhost:9000"}, false},
		{"r2://devstrap-test?endpoint=http://localhost:9000&region=us-east-1", hubSpec{scheme: "r2", bucket: "devstrap-test", endpoint: "http://localhost:9000", region: "us-east-1"}, false},
		{"r2://devstrap-test?region=auto", hubSpec{scheme: "r2", bucket: "devstrap-test", region: "auto"}, false},
		{"r2://user:key@bucket", hubSpec{}, true}, // credentials must not ride the URI
		{"r2://", hubSpec{}, true},                // no bucket
		{"file:///tmp/x", hubSpec{}, true},        // wrong scheme
		{"", hubSpec{}, true},                     // empty
	}
	for _, c := range cases {
		got, err := parseHubURI(c.uri)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("parseHubURI(%q) = nil error, want error", c.uri)
		case c.wantErr:
			// expected error; spec ignored
		case err != nil:
			t.Errorf("parseHubURI(%q) = %v, want nil", c.uri, err)
		case got != c.wantSpec:
			t.Errorf("parseHubURI(%q) = %+v, want %+v", c.uri, got, c.wantSpec)
		}
	}
}

func TestHubConfigured(t *testing.T) {
	cases := []struct {
		name    string
		hubFile string
		setHub  string
		wantErr bool
	}{
		{"hub-file set", "/tmp/hub.json", "", false},
		{"r2 uri", "", "r2://devstrap-test", false},
		{"r2 uri with endpoint", "", "r2://devstrap-test?endpoint=http://localhost:9000", false},
		{"file uri", "", "file:/tmp/hub.json", false},
		{"bad r2 uri", "", "r2://", true},
		{"no hub", "", "", true},
		{"unrecognized scheme", "", "ftp://x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := &options{v: viper.New()}
			if c.setHub != "" {
				opts.v.Set("hub", c.setHub)
			}
			err := hubConfigured(opts, c.hubFile)
			if c.wantErr && err == nil {
				t.Errorf("hubConfigured(%s): want error, got nil", c.name)
			}
			if !c.wantErr && err != nil {
				t.Errorf("hubConfigured(%s): want nil, got %v", c.name, err)
			}
		})
	}
}
