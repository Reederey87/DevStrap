package cli

import (
	"context"

	"github.com/Reederey87/DevStrap/internal/state"
)

func sweepStaleAgentRuns(ctx context.Context, store *state.Store) (reconciled int, stillRunning int, err error) {
	runs, err := store.RunningAgentRunsWithPID(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, run := range runs {
		if processIdentityAlive(run.RunnerPID, run.RunnerStartedAt) {
			continue
		}
		if err := store.UpdateAgentRunStatus(ctx, run.ID, "interrupted"); err != nil {
			return reconciled, 0, err
		}
		reconciled++
	}
	stillRunning, err = store.CountAgentRunsByStatus(ctx, "running")
	if err != nil {
		return reconciled, 0, err
	}
	return reconciled, stillRunning, nil
}
