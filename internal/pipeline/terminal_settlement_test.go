package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalSettlement_UnavailableLockDoesNotCommit(t *testing.T) {
	d, _, run, _ := setupTest(t)
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := AcquireCommitStatusLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { <-CommitStatusLock }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := RecordRunFailure(ctx, d, run.ID, "independent failure", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unavailable lock error lost: %v", err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("unavailable publication boundary committed terminal state: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_StorageErrorDoesNotEmitCommittedEvent(t *testing.T) {
	d, p, run, repo := setupTest(t)
	before := run.Status
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	terminal := false
	executor := NewExecutor(d, p, nil, nil, nil, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			terminal = true
		}
	})
	failure := errors.New("independent failure")
	err := executor.failRun(run, repo, failure)
	if !errors.Is(err, failure) || err == failure {
		t.Errorf("storage failure not propagated with original identity: %v", err)
	}
	if terminal || run.Status != before {
		t.Fatalf("failed storage produced a committed terminal outcome: event=%v,status=%s", terminal, run.Status)
	}
}
