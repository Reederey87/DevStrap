package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/cli"
	"github.com/rogpeppe/go-internal/testscript"
)

// TestMain registers the real devstrap entrypoint so testscript can drive it as
// a subprocess, exercising argv parsing, cli.Execute, and the os.Exit/ExitCode
// contract that the in-process cobra tests bypass.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"devstrap": func() {
			os.Exit(cli.ExitCode(cli.Execute(context.Background())))
		},
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"exitstatus": exitStatus,
			"waitfile":   waitFile,
		},
		Setup: func(env *testscript.Env) error {
			// Keep daemon socket paths below sockaddr_un.sun_path's 104-byte
			// darwin limit (108 on linux). A home under testscript's long $WORK
			// path fails with EINVAL, which is deliberately not "unavailable";
			// internal/daemon's tempSocketPath and cli.TestDaemonStartServesAndStops
			// use the same short-temp-dir precedent.
			dir, err := os.MkdirTemp("", "dsh")
			if err != nil {
				return err
			}
			env.Defer(func() { _ = os.RemoveAll(dir) })
			env.Vars = append(env.Vars, "SHORTHOME="+dir)
			return nil
		},
	})
}

func waitFile(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("waitfile does not support neg")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: waitfile file")
	}
	path := ts.MkAbs(args[0])
	for range 500 {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !os.IsNotExist(err) {
			ts.Fatalf("unexpected stat error: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ts.Fatalf("timed out waiting for %q to be created", path)
}

func exitStatus(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("status does not support neg")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: status code command [args...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil || want < 0 {
		ts.Fatalf("invalid exit status %q", args[0])
	}
	err = ts.Exec(args[1], args[2:]...)
	got := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			ts.Fatalf("%s failed without an exit status: %v", args[1], err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		ts.Fatalf("%s exit status = %d, want %d", args[1], got, want)
	}
}
