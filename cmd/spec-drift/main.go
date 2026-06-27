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
	flag.StringVar(&base, "base", "origin/main", "base ref for changed-file detection")
	flag.StringVar(&head, "head", "HEAD", "head ref for changed-file detection")
	flag.StringVar(&repo, "repo", ".", "repository root")
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
	if report.OK() {
		fmt.Printf("spec drift check passed: %d specs, %d changed files\n", len(report.Specs), len(report.ChangedFiles))
		return
	}
	fmt.Fprintln(os.Stderr, "spec drift check failed:")
	for _, finding := range report.Findings {
		fmt.Fprintf(os.Stderr, "- %s\n", finding)
	}
	os.Exit(1)
}
