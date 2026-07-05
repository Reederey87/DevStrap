package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
)

type fakeSandbox struct {
	availableErr error
}

func (f fakeSandbox) Name() string     { return "fake-sandbox" }
func (f fakeSandbox) Available() error { return f.availableErr }
func (f fakeSandbox) Command(_ context.Context, _ platform.SandboxSpec, argv []string) ([]string, func(), error) {
	return append([]string{"fake-sandbox-exec"}, argv...), func() {}, nil
}

func withFakeSandbox(t *testing.T, sb platform.Sandbox) {
	t.Helper()
	prev := sandboxBackend
	sandboxBackend = func() platform.Sandbox { return sb }
	t.Cleanup(func() { sandboxBackend = prev })
}

func TestResolveAgentSandboxMatrix(t *testing.T) {
	unavailable := fakeSandbox{availableErr: errors.New("no adapter on this host")}
	available := fakeSandbox{}

	cases := []struct {
		name        string
		mode        string
		policy      string
		sandbox     platform.Sandbox
		wantEnabled bool
		wantDenyNet bool
		wantErrCode int
		wantWarn    bool
	}{
		{name: "auto available guarded", mode: "auto", policy: "guarded", sandbox: available, wantEnabled: true},
		{name: "auto available readonly denies network", mode: "auto", policy: "readonly", sandbox: available, wantEnabled: true, wantDenyNet: true},
		{name: "auto available cautious denies network", mode: "auto", policy: "cautious", sandbox: available, wantEnabled: true, wantDenyNet: true},
		{name: "auto available ephemeral-ci keeps network", mode: "auto", policy: "ephemeral-ci", sandbox: available, wantEnabled: true},
		{name: "auto unavailable warns and degrades", mode: "auto", policy: "guarded", sandbox: unavailable, wantWarn: true},
		{name: "require unavailable is a policy error", mode: "require", policy: "guarded", sandbox: unavailable, wantErrCode: exitPolicy},
		{name: "require available", mode: "require", policy: "guarded", sandbox: available, wantEnabled: true},
		{name: "off never sandboxes", mode: "off", policy: "guarded", sandbox: available},
		{name: "yolo-local always unconfined", mode: "auto", policy: "yolo-local", sandbox: available},
		{name: "yolo-local conflicts with require", mode: "require", policy: "yolo-local", sandbox: available, wantErrCode: exitInvalidConfig},
		{name: "bogus mode is a usage error", mode: "sometimes", policy: "guarded", sandbox: available, wantErrCode: exitInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSandbox(t, tc.sandbox)
			var stderr bytes.Buffer
			launch, err := resolveAgentSandbox(tc.mode, tc.policy, &stderr, "/tmp/devstrap-home")
			if tc.wantErrCode != 0 {
				var app appError
				if !errors.As(err, &app) || app.code != tc.wantErrCode {
					t.Fatalf("err = %v, want appError code %d", err, tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAgentSandbox: %v", err)
			}
			if launch.enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", launch.enabled, tc.wantEnabled)
			}
			if launch.denyNetwork != tc.wantDenyNet {
				t.Fatalf("denyNetwork = %v, want %v", launch.denyNetwork, tc.wantDenyNet)
			}
			warned := strings.Contains(stderr.String(), "OS sandbox unavailable")
			if warned != tc.wantWarn {
				t.Fatalf("warn printed = %v (stderr %q), want %v", warned, stderr.String(), tc.wantWarn)
			}
			if tc.wantEnabled && launch.sandbox == nil {
				t.Fatal("enabled launch has nil sandbox")
			}
		})
	}
}
