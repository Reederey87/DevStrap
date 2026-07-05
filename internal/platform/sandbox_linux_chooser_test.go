//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type chooserStub struct {
	name string
	err  error
}

func (c chooserStub) Name() string     { return c.name }
func (c chooserStub) Available() error { return c.err }
func (c chooserStub) Command(context.Context, SandboxSpec, []string) ([]string, func(), error) {
	return nil, func() {}, nil
}

func TestChooseLinuxSandbox(t *testing.T) {
	bwrapErr := fmt.Errorf("%w: bwrap down", ErrUnsupported)
	landlockErr := fmt.Errorf("%w: landlock down", ErrUnsupported)

	cases := []struct {
		name            string
		backend         string
		bwrapErr        error
		landlockErr     error
		wantName        string
		wantErr         error
		wantUnsupported bool
		wantErrSub      []string
	}{
		{name: "auto prefers bwrap", wantName: "bwrap"},
		{name: "auto falls back to landlock", bwrapErr: bwrapErr, wantName: "landlock"},
		{name: "auto reports both failures", bwrapErr: bwrapErr, landlockErr: landlockErr, wantUnsupported: true, wantErrSub: []string{"bwrap down", "landlock down"}},
		{name: "forced bwrap returns its own error", backend: "bwrap", bwrapErr: bwrapErr, wantName: "bwrap", wantErr: bwrapErr},
		{name: "forced bubblewrap", backend: "bubblewrap", wantName: "bwrap"},
		{name: "forced landlock", backend: "landlock", wantName: "landlock"},
		{name: "forced landlock returns its own error", backend: "landlock", landlockErr: landlockErr, wantName: "landlock", wantErr: landlockErr},
		{name: "forced landlock trims and folds", backend: " LANDLOCK ", wantName: "landlock"},
		{name: "bogus backend", backend: "bogus", wantErrSub: []string{SandboxBackendEnv, "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseLinuxSandbox(tc.backend, chooserStub{name: "bwrap", err: tc.bwrapErr}, chooserStub{name: "landlock", err: tc.landlockErr})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want stub err %v", err, tc.wantErr)
				}
			} else if tc.wantUnsupported {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("err = %v, want ErrUnsupported", err)
				}
			} else if len(tc.wantErrSub) > 0 {
				if err == nil {
					t.Fatal("err = nil, want error")
				}
			} else if err != nil {
				t.Fatalf("chooseLinuxSandbox: %v", err)
			}
			if err != nil {
				for _, sub := range tc.wantErrSub {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("err = %v, want substring %q", err, sub)
					}
				}
			}
			if tc.wantName == "" {
				if got != nil {
					t.Fatalf("selected = %s, want nil", got.Name())
				}
				return
			}
			if got == nil || got.Name() != tc.wantName {
				t.Fatalf("selected = %v, want %s", got, tc.wantName)
			}
		})
	}
}
