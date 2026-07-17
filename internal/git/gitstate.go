package git

import (
	"context"
	"fmt"
	"strings"
)

// Gitstate is a read-only snapshot of a repository's working state (spec/07,
// working-state validation plane Layer A): branch, HEAD, upstream tracking,
// and dirty/untracked/unmerged/ahead/behind/stash counts. It backs the
// repo.gitstate.observed sync event.
type Gitstate struct {
	Branch         string
	HeadSHA        string
	UpstreamBranch string
	UpstreamSHA    string
	DirtyCount     int
	UntrackedCount int
	UnmergedCount  int
	AheadCount     int
	BehindCount    int
	StashCount     int
}

// CaptureGitstate captures dir's working-state snapshot with
// `git --no-optional-locks status --porcelain=v2 --branch`, which never
// writes .git/index (Layer A's defining property: capture must be safe to run
// against a repo another process is using). Porcelain v2 does not report the
// upstream SHA or stash count, so those are resolved with two follow-up
// calls; either failing (no upstream, no stash) leaves the corresponding
// field at its zero value rather than failing the whole capture.
func (r Runner) CaptureGitstate(ctx context.Context, dir string) (Gitstate, error) {
	out, err := r.Run(ctx, dir, "--no-optional-locks", "status", "--porcelain=v2", "--branch")
	if err != nil {
		return Gitstate{}, err
	}
	gs := parsePorcelainV2(out)

	if gs.UpstreamBranch != "" {
		if sha, err := r.Run(ctx, dir, "rev-parse", "--verify", "-q", gs.UpstreamBranch); err == nil {
			gs.UpstreamSHA = sha
		}
	}

	stashOut, err := r.Run(ctx, dir, "stash", "list")
	if err != nil {
		return Gitstate{}, err
	}
	gs.StashCount = countNonEmptyLines(stashOut)

	return gs, nil
}

// parsePorcelainV2 parses `git status --porcelain=v2 --branch` output into a
// Gitstate. Unrecognized lines are ignored so a future porcelain v2 addition
// degrades gracefully instead of erroring.
func parsePorcelainV2(out string) Gitstate {
	var gs Gitstate
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			if head := strings.TrimPrefix(line, "# branch.head "); head != "(detached)" {
				gs.Branch = head
			}
		case strings.HasPrefix(line, "# branch.oid "):
			if oid := strings.TrimPrefix(line, "# branch.oid "); oid != "(initial)" {
				gs.HeadSHA = oid
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			gs.UpstreamBranch = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			var ahead, behind int
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, "# branch.ab "), "+%d -%d", &ahead, &behind); err == nil {
				gs.AheadCount = ahead
				gs.BehindCount = behind
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			gs.DirtyCount++
		case strings.HasPrefix(line, "u "):
			gs.UnmergedCount++
		case strings.HasPrefix(line, "? "):
			gs.UntrackedCount++
		}
	}
	return gs
}

func countNonEmptyLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
