package fold

import "testing"

// foldStream folds a slice of content hashes starting at seq 1 from the seed.
func foldStream(ws, dev string, hashes []string) State {
	f := Seed(ws, dev)
	for i, h := range hashes {
		f = Step(f, int64(i+1), h)
	}
	return f
}

// TestFoldDeterministic: the same stream folds to the same value.
func TestFoldDeterministic(t *testing.T) {
	hashes := []string{"sha256:aa", "sha256:bb", "sha256:cc"}
	a := foldStream("ws", "dev", hashes)
	b := foldStream("ws", "dev", hashes)
	if a != b {
		t.Fatalf("fold is not deterministic: %x != %x", a, b)
	}
}

// TestFoldPrefixCommitment: truncating the newest event changes the fold, so a
// commitment to seq N cannot be satisfied by a prefix of length N-1 (the
// tail-omission property).
func TestFoldPrefixCommitment(t *testing.T) {
	full := foldStream("ws", "dev", []string{"sha256:a", "sha256:b", "sha256:c"})
	short := foldStream("ws", "dev", []string{"sha256:a", "sha256:b"})
	if full == short {
		t.Fatal("fold at seq 3 must differ from fold at seq 2")
	}
}

// TestFoldForkDetection: two histories that both reach seq N but differ at some
// position produce different folds (the equivocation/fork property).
func TestFoldForkDetection(t *testing.T) {
	a := foldStream("ws", "dev", []string{"sha256:a", "sha256:X", "sha256:c"})
	b := foldStream("ws", "dev", []string{"sha256:a", "sha256:Y", "sha256:c"})
	if a == b {
		t.Fatal("streams that diverge at seq 2 must fold differently at seq 3")
	}
}

// TestFoldPositionBinding: swapping two events (same set, different order)
// changes the fold, because each step binds its seq.
func TestFoldPositionBinding(t *testing.T) {
	a := foldStream("ws", "dev", []string{"sha256:a", "sha256:b"})
	b := foldStream("ws", "dev", []string{"sha256:b", "sha256:a"})
	if a == b {
		t.Fatal("reordered stream must fold differently")
	}
}

// TestFoldDeviceScoped: the same content/seq under a different (workspace,
// device) seed folds differently, so a fold cannot be replayed across streams.
func TestFoldDeviceScoped(t *testing.T) {
	hashes := []string{"sha256:a", "sha256:b"}
	if foldStream("ws", "dev1", hashes) == foldStream("ws", "dev2", hashes) {
		t.Fatal("different device seeds must fold differently")
	}
	if foldStream("ws1", "dev", hashes) == foldStream("ws2", "dev", hashes) {
		t.Fatal("different workspace seeds must fold differently")
	}
}

// TestEncodeDecodeRoundTrip: Encode/Decode round-trips and rejects malformed
// input.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	f := foldStream("ws", "dev", []string{"sha256:a"})
	enc := Encode(f)
	got, ok, err := Decode(enc)
	if err != nil || !ok {
		t.Fatalf("Decode(%q) = ok=%v err=%v", enc, ok, err)
	}
	if got != f {
		t.Fatalf("round-trip mismatch: %x != %x", got, f)
	}
	if _, ok, _ := Decode(""); ok {
		t.Fatal(`Decode("") should report ok=false`)
	}
	if _, _, err := Decode("nothex"); err == nil {
		t.Fatal("Decode of a non-prefixed string should error")
	}
	if _, _, err := Decode("sha256:zz"); err == nil {
		t.Fatal("Decode of invalid hex should error")
	}
}
