package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Reederey87/DevStrap/internal/specdrift"
)

func main() {
	var base string
	var head string
	var repo string
	var advisory bool
	flag.StringVar(&base, "base", "origin/main", "base ref for changed-file detection")
	flag.StringVar(&head, "head", "HEAD", "head ref for changed-file detection")
	flag.StringVar(&repo, "repo", ".", "repository root")
	flag.BoolVar(&advisory, "advisory", false, "report findings as GitHub Actions warnings and exit 0 (fork PRs, AD-8)")
	flag.Parse()

	report, err := specdrift.Check(context.Background(), specdrift.Options{
		RepoRoot:       repo,
		Base:           base,
		Head:           head,
		RequireWorkLog: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "spec drift check failed: %v\n", err)
		os.Exit(1)
	}
	if specdrift.PrintReport(os.Stdout, os.Stderr, report, advisory) {
		os.Exit(1)
	}
}
