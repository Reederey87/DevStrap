package sync

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestMain(m *testing.M) {
	keyring.MockInit()
	// Deterministic sync tests use tiny synthetic physical HLC values.
	epochFloorMS = 0
	os.Exit(m.Run())
}
