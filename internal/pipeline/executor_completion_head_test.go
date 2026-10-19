package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func completionWorktree(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}} {
		if _, err := git.Run(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git.CommitAll(ctx, dir, "first"); err != nil {
		t.Fatal(err)
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, head
}

func TestExecutorCompletionRequiresExactCleanReceiptHead(t *testing.T) {
	for _, scenario := range []string{"current", "descendant", "dirty", "missing-receipt", "missing-head", "unavailable-head"} {
		t.Run(scenario, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir, head := completionWorktree(t)
			run.HeadSHA = head
			if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-receipt" {
				if _, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
					LocalHeadSHA: head, TargetBranch: "main", TargetSHA: head,
					MergeBaseSHA: head, DiffDigest: strings.Repeat("a", 64),
				}, types.StepRebase); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "descendant" || scenario == "dirty" {
				if err := os.WriteFile(filepath.Join(workDir, "file.txt"), []byte("second"), 0o644); err != nil {
					t.Fatal(err)
				}
				if scenario == "descendant" {
					if err := git.CommitAll(context.Background(), workDir, "second"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "missing-head" {
				run.HeadSHA = ""
				if err := database.UpdateRunHeadSHA(run.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unavailable-head" {
				if err := os.RemoveAll(filepath.Join(workDir, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			events := &eventCollector{}
			executor := NewExecutor(database, p, nil, nil, nil, events.handler)
			executor.workDir = workDir
			mergedCalls := 0
			merged := "merged"
			run.PRState = &merged
			executor.SetOnPRMerged(func(context.Context, string) { mergedCalls++ })
			err := executor.completeRun(context.Background(), run, repo)
			current, dbErr := database.GetRun(run.ID)
			if dbErr != nil {
				t.Fatal(dbErr)
			}
			if scenario == "current" {
				if err != nil || current.Status != types.RunCompleted || current.HeadSHA != head || current.TerminalHeadVerifiedAt == nil || mergedCalls != 1 {
					t.Fatalf("completion: err=%v run=%+v merged calls=%d", err, current, mergedCalls)
				}
				if event := events.findRunEvent(ipc.EventRunCompleted); event == nil || event.Status == nil || *event.Status != string(types.RunCompleted) {
					t.Fatalf("completion event = %+v", event)
				}
			} else if err == nil || current.Status == types.RunCompleted || current.TerminalHeadVerifiedAt != nil || mergedCalls != 0 || events.findRunEvent(ipc.EventRunCompleted) != nil {
				t.Fatalf("unverified completion: err=%v run=%+v merged calls=%d", err, current, mergedCalls)
			}
		})
	}
}

func TestExecutorCompletionRawShutdownPreservesPreparationError(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	if err := os.RemoveAll(filepath.Join(workDir, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := RecordRawShutdown(context.Background(), database, []string{run.ID}, "runtime", func() {}); err != nil {
		t.Fatal(err)
	}
	events := &eventCollector{}
	executor := NewExecutor(database, p, nil, nil, nil, events.handler)
	executor.workDir = workDir
	prepErr := executor.completeRun(context.Background(), run, repo)
	if prepErr == nil || !strings.Contains(prepErr.Error(), "read worktree head before completion") {
		t.Fatalf("raw supersession swallowed Git preparation: %v", prepErr)
	}
	result := executor.FailRun(run, repo, prepErr)
	if !errors.Is(result, prepErr) {
		t.Fatalf("original preparation error identity lost: %v", result)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != prepErr.Error() {
		t.Fatalf("actual preparation failure lost: %+v,%v", stored, err)
	}
}

func TestExecutorCompletionRawOutcomeEmitsOnce(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	terminalEvents := 0
	executor := NewExecutor(database, p, nil, nil, []Step{&adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		if err := RecordRawShutdown(context.Background(), database, []string{run.ID}, "runtime", func() {}); err != nil {
			return nil, err
		}
		return &StepOutcome{}, nil
	}}}, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			terminalEvents++
		}
	})
	err := executor.Execute(context.Background(), run, repo, workDir)
	if err == nil || err.Error() != db.RawShutdownReason {
		t.Fatalf("untruthful raw completion result: %v", err)
	}
	stored, dbErr := database.GetRun(run.ID)
	if dbErr != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != db.RawShutdownReason || terminalEvents != 1 {
		t.Fatalf("raw completion: %+v,%v events=%d", stored, dbErr, terminalEvents)
	}
}
