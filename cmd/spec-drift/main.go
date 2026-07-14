package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Reederey87/DevStrap/internal/specdrift"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args and executes the spec-drift check, writing to stdout/stderr
// and returning the process exit code. Split out from main so it is testable
// without os.Exit tearing down the test binary.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spec-drift", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var base, head, repo string
	var advisory bool
	fs.StringVar(&base, "base", "origin/main", "base ref for changed-file detection")
	fs.StringVar(&head, "head", "HEAD", "head ref for changed-file detection")
	fs.StringVar(&repo, "repo", ".", "repository root")
	fs.BoolVar(&advisory, "advisory", false, "report findings as GitHub Actions warnings and exit 0 (fork PRs, AD-8)")
	if err := fs.Parse(args); err != nil {
		// Match stdlib flag.CommandLine's ExitOnError behavior: -h/--help is
		// not a usage error and exits 0, unlike every other parse failure.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	report, err := specdrift.Check(context.Background(), specdrift.Options{
		RepoRoot:       repo,
		Base:           base,
		Head:           head,
		RequireWorkLog: true,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spec drift check failed: %v\n", err)
		return 1
	}
	if specdrift.PrintReport(stdout, stderr, report, advisory) {
		return 1
	}
	return 0
}
