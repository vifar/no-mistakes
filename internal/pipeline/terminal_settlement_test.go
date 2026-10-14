package pipeline

import (
	"context"
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalSettlement_LockReleaseCompletesOwnedDuplicateWrites(t *testing.T) {
	d, p, run, _ := setupTest(t)
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := AcquireCommitStatusLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(ReleaseCommitStatusLock) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- RecordRunFailure(ctx, d, run.ID, "independent failure", nil) }()
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-results:
		t.Fatalf("settlement abandoned its owner: %v", err)
	default:
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("terminal write crossed held lock: %+v,%v", stored, err)
	}
	release()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("released lock did not settle owned failure")
		}
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err = reopened.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent failure" {
		t.Fatalf("reopened settlement: %+v,%v", stored, err)
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

func TestTerminalSettlement_CompletionCannotOverwriteCommittedFailure(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRunFailure(context.Background(), d, run.ID, "daemon shutting down", nil); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(d, p, nil, nil, nil, nil)
	if err := executor.completeRun(run, repo); err == nil {
		t.Error("completion claimed success after committed failure")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "daemon shutting down" || run.Status != types.RunFailed {
		t.Fatalf("completion replaced raw terminal decision: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_StartupCannotResurrectTerminalRun(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRunFailure(context.Background(), d, run.ID, "independent failure", nil); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(d, p, nil, nil, nil, nil)
	_ = executor.Execute(context.Background(), run, repo, t.TempDir())
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent failure" {
		t.Fatalf("startup resurrected committed failure: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_RegisteredStartupAfterRawSupersession(t *testing.T) {
	d, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if err := RecordRawShutdown(context.Background(), d, []string{run.ID}, "runtime", func() { cancel(errors.New(db.RawShutdownReason)) }); err != nil {
		t.Fatal(err)
	}
	step := newPassStep(types.StepCI)
	executor := NewExecutor(d, p, nil, nil, []Step{step}, nil)
	_ = executor.Execute(ctx, run, repo, workDir)
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != db.RawShutdownReason || step.callCount() != 0 {
		t.Fatalf("registered executor escaped raw supersession: %+v,%v,calls=%d", stored, err, step.callCount())
	}
}
