package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

// P6-SEC-03 contiguity guard: approving from a device whose own keyring is
// incomplete would grant an incomplete key set and strand the approved device
// on the gap. The guard refuses BEFORE any trust write; --allow-epoch-gap
// overrides; a keyless device (the P4-SEC-04 founder-pinning ceremony) is
// never blocked.

func initGuardHome(t *testing.T) (home, root string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	return home, root
}

func withGuardStore(t *testing.T, home string, fn func(ctx context.Context, st *state.Store)) {
	t.Helper()
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	st, err := state.Open(context.Background(), opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	fn(context.Background(), st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func enrollPendingDevice(t *testing.T, home, root, id string) {
	t.Helper()
	ageID, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "enroll", id, "--name", id, "--os", "linux", "--arch", "arm64",
		"--age-recipient", ageID.Recipient, "--signing-public-key", signing.Public); err != nil {
		t.Fatalf("enroll pending: %v (%s)", err, stderr)
	}
}

func trustStateOf(t *testing.T, home, id string) string {
	t.Helper()
	var got string
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		devices, err := st.ListDevices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devices {
			if d.ID == id {
				got = d.TrustState
				return
			}
		}
	})
	return got
}

func TestApproveRefusesHeldEpochGap(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := initGuardHome(t)
	enrollPendingDevice(t, home, root, "dev_gap_target")
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		// Held epochs 1 and 3: epoch 2's grant never arrived.
		if err := st.RecordKeyEpoch(ctx, 1, "kid1", "self"); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordKeyEpoch(ctx, 3, "kid3", "grant"); err != nil {
			t.Fatal(err)
		}
	})

	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "approve", "dev_gap_target")
	if err == nil {
		t.Fatal("approve with an epoch gap succeeded, want refusal")
	}
	if !strings.Contains(stderr, "missing workspace key epoch(s) 2") || !strings.Contains(stderr, "--allow-epoch-gap") {
		t.Fatalf("stderr = %q, want the gap named and the override offered", stderr)
	}
	if got := trustStateOf(t, home, "dev_gap_target"); got != "pending" {
		t.Fatalf("trust state after refusal = %q, want pending (no DB write)", got)
	}

	// The override approves anyway (grant wrap fails on the missing custody
	// slots with a warning, but the trust write itself must land).
	if _, _, err := executeForTest("--home", home, "--root", root, "devices", "approve", "dev_gap_target", "--allow-epoch-gap"); err != nil {
		t.Fatalf("approve --allow-epoch-gap: %v", err)
	}
	if got := trustStateOf(t, home, "dev_gap_target"); got != "approved" {
		t.Fatalf("trust state after override = %q, want approved", got)
	}
}

func TestApproveRefusesOpenKeyGrantWait(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := initGuardHome(t)
	enrollPendingDevice(t, home, root, "dev_wait_target")
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		if err := st.RecordKeyEpoch(ctx, 1, "kid1", "self"); err != nil {
			t.Fatal(err)
		}
		// This device has SEEN epoch-2 ciphertext but holds no key for it.
		if _, err := st.NoteMissingKeyGrant(ctx, 2, ""); err != nil {
			t.Fatal(err)
		}
	})

	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "approve", "dev_wait_target")
	if err == nil {
		t.Fatal("approve with an open key-grant wait succeeded, want refusal")
	}
	if !strings.Contains(stderr, "awaiting key grant(s) for epoch(s) 2") {
		t.Fatalf("stderr = %q, want the open wait named", stderr)
	}
	if got := trustStateOf(t, home, "dev_wait_target"); got != "pending" {
		t.Fatalf("trust state after refusal = %q, want pending", got)
	}
}

// A wait row whose kid is shorter than the display prefix must not panic the
// guard's refusal formatting (the kid rode an unauthenticated hub envelope, so
// its length is hostile input — post-#55 Codex review).
func TestApproveGuardShortKidLabelDoesNotPanic(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := initGuardHome(t)
	enrollPendingDevice(t, home, root, "dev_short_kid")
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		if err := st.RecordKeyEpoch(ctx, 1, "kid1", "self"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.NoteMissingKeyGrant(ctx, 1, "ab"); err != nil {
			t.Fatal(err)
		}
	})
	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "approve", "dev_short_kid")
	if err == nil {
		t.Fatal("approve with an open short-kid wait succeeded, want refusal")
	}
	if !strings.Contains(stderr, "kid ab") {
		t.Fatalf("stderr = %q, want the short kid named without panicking", stderr)
	}
}

func TestKeylessApproveSkipsGuard(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := initGuardHome(t)
	// Even with an open wait, a KEYLESS device passes: it grants nothing on
	// approve — this approval is the founder-pinning ceremony (P4-SEC-04) and
	// must stay friction-free for a joiner that synced before pinning.
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		if _, err := st.NoteMissingKeyGrant(ctx, 1, ""); err != nil {
			t.Fatal(err)
		}
	})
	ageID, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "enroll", "dev_founder", "--name", "founder", "--os", "darwin", "--arch", "arm64",
		"--age-recipient", ageID.Recipient, "--signing-public-key", signing.Public, "--approve")
	if err != nil {
		t.Fatalf("keyless enroll --approve: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "enrolled as approved") {
		t.Fatalf("stdout = %q, want approval", stdout)
	}
}

func TestEnrollApproveRefusesGapWithNoDeviceWrite(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root := initGuardHome(t)
	withGuardStore(t, home, func(ctx context.Context, st *state.Store) {
		if err := st.RecordKeyEpoch(ctx, 2, "kid2", "grant"); err != nil {
			t.Fatal(err) // epoch 1 missing entirely
		}
	})
	ageID, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, execErr := executeForTest("--home", home, "--root", root,
		"devices", "enroll", "dev_new", "--name", "new", "--os", "linux", "--arch", "arm64",
		"--age-recipient", ageID.Recipient, "--signing-public-key", signing.Public, "--approve")
	if execErr == nil {
		t.Fatal("enroll --approve with an epoch gap succeeded, want refusal")
	}
	if !strings.Contains(stderr, "missing workspace key epoch(s) 1") {
		t.Fatalf("stderr = %q, want missing epoch 1 named", stderr)
	}
	if got := trustStateOf(t, home, "dev_new"); got != "" {
		t.Fatalf("device row exists with trust %q after refusal, want no row at all", got)
	}
}
