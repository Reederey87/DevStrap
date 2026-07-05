package platform

import "testing"

func TestParseSeatbeltDenials(t *testing.T) {
	logText := `2026-07-05 12:00:00.000 host kernel: Sandbox: devstrap-agent(1234) deny(1) file-write-create /Users/x/outside devstrap-sb-arun_1
garbage line
2026-07-05 12:00:01.000 host kernel: Sandbox: devstrap-agent(1234) deny(1) file-read-data /Users/x/.ssh/id_ed25519 devstrap-sb-arun_1
2026-07-05 12:00:02.000 host kernel: unrelated allow file-read-data /tmp/ok
2026-07-05 12:00:03.000 host kernel: Sandbox: devstrap-agent(1234) deny(1) network-outbound <private> devstrap-sb-arun_1`
	got := parseSeatbeltDenials(logText)
	want := []SandboxViolation{
		{Operation: "file-write-create", Path: "/Users/x/outside"},
		{Operation: "file-read-data", Path: "/Users/x/.ssh/id_ed25519"},
		{Operation: "network-outbound", Path: "<private>"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].Operation != want[i].Operation || got[i].Path != want[i].Path {
			t.Fatalf("row %d = %+v, want operation=%q path=%q", i, got[i], want[i].Operation, want[i].Path)
		}
		if got[i].Detail == "" {
			t.Fatalf("row %d missing raw detail", i)
		}
	}
}

func TestParseSeatbeltDenialsSkipsEmptyAndGarbage(t *testing.T) {
	if got := parseSeatbeltDenials(""); got != nil {
		t.Fatalf("empty input = %+v, want nil", got)
	}
	if got := parseSeatbeltDenials("not a denial\ndeny( but malformed"); got != nil {
		t.Fatalf("garbage input = %+v, want nil", got)
	}
}
