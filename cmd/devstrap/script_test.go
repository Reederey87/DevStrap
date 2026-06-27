package main

import (
	"context"
	"os"
	"testing"

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
	})
}
