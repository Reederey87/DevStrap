package id

import (
	"strings"
	"testing"
)

func TestNewMintsValidIDs(t *testing.T) {
	for _, prefix := range []string{"ws", "dev", "proj"} {
		got, err := New(prefix)
		if err != nil {
			t.Fatalf("New(%q): %v", prefix, err)
		}
		if !Valid(prefix, got) {
			t.Fatalf("New(%q) = %q, not Valid for its own prefix", prefix, got)
		}
	}
}

func TestNewRejectsBadPrefixes(t *testing.T) {
	for _, prefix := range []string{"", " ", "a_b", "a b", "a/b"} {
		if _, err := New(prefix); err == nil {
			t.Fatalf("New(%q) succeeded, want error", prefix)
		}
	}
}

func TestValid(t *testing.T) {
	hex32 := strings.Repeat("0123456789abcdef", 2)
	cases := []struct {
		name   string
		prefix string
		value  string
		want   bool
	}{
		{"canonical", "ws", "ws_" + hex32, true},
		{"other prefix canonical", "dev", "dev_" + hex32, true},
		{"empty value", "ws", "", false},
		{"prefix only", "ws", "ws_", false},
		{"wrong prefix", "ws", "dev_" + hex32, false},
		{"missing separator", "ws", "ws" + hex32, false},
		{"too short", "ws", "ws_" + hex32[:31], false},
		{"too long", "ws", "ws_" + hex32 + "0", false},
		{"uppercase hex", "ws", "ws_" + strings.ToUpper(hex32), false},
		{"non-hex letter", "ws", "ws_" + strings.Repeat("g", 32), false},
		{"embedded dash", "ws", "ws_0123456789ab-def0123456789abcdef", false},
		{"loose test id", "ws", "ws_test", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.prefix, tc.value); got != tc.want {
				t.Fatalf("Valid(%q, %q) = %v, want %v", tc.prefix, tc.value, got, tc.want)
			}
		})
	}
}
