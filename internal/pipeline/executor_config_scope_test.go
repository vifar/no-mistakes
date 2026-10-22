package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIssue131FailsTheStepThatChangesSharedConfig(t *testing.T) {
	database, p, run, repo := setupTest(t)
	seed := completionFixture(t, database, run)
	ctx := context.Background()
	bare := p.RepoDir(repo.ID)
	if err := git.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, seed, "push", bare, "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if err := git.IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := git.RecordSharedConfigBaseline(ctx, bare); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "run")
	if err := git.WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bare, "config")
	var later bool
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			// Simulate a tool bypassing git's config lock with a direct file write.
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return nil, err
			}
			_, err = f.WriteString("\n[core]\n\tbare = true\n")
			closeErr := f.Close()
			if err != nil {
				return nil, err
			}
			return &StepOutcome{}, closeErr
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) { later = true; return &StepOutcome{}, nil }},
	}, nil)
	err := executor.Execute(ctx, run, repo, wt)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "core.bare") {
		t.Fatalf("want shared file/key failure, got %v", err)
	}
	if later {
		t.Fatal("later Document ran after Review polluted config")
	}
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepReview && row.Status != types.StepStatusFailed {
			t.Fatalf("writer Review status = %s", row.Status)
		}
	}
}

func TestIssue131MissingGateFailsPreflightBeforeStep(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		name := "missing-gate"
		if enrolled {
			name = "missing-baseline"
		}
		t.Run(name, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "gate.git")
			if enrolled {
				if err := git.InitBare(context.Background(), gate); err != nil {
					t.Fatal(err)
				}
			}
			executed := false
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
				executed = true
				return &StepOutcome{}, nil
			}}
			_, err := executeWithConfigProtection(step, &StepContext{Ctx: context.Background(), GateDir: gate})
			if err == nil || !strings.Contains(err.Error(), "review shared config preflight") || !strings.Contains(err.Error(), gate) {
				t.Fatalf("expected named preflight failure for missing gate enrollment, got %v", err)
			}
			if executed {
				t.Fatal("step ran without an enrolled gate")
			}
		})
	}
}

func TestIssue131GuardDoesNotRegisterSubmodulesBeforePreparation(t *testing.T) {
	ctx := context.Background()
	gate := filepath.Join(t.TempDir(), "gate.git")
	if err := git.InitBare(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := git.IsolateHooksPath(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := git.RecordSharedConfigBaseline(ctx, gate); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if _, err := git.Run(ctx, workDir, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".gitmodules"), []byte("[submodule \"dependency\"]\npath = dependency\nurl = https://example.com/old.git\nupdate = none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview} {
		step := &adaptiveCallStep{name: name, fn: func(*StepContext) (*StepOutcome, error) {
			for _, key := range []string{"submodule.dependency.url", "submodule.dependency.update"} {
				if _, err := git.Run(ctx, workDir, "config", "--get", key); err == nil {
					t.Fatalf("%s registered %s before preparation", name, key)
				}
			}
			return &StepOutcome{}, nil
		}}
		if _, err := executeWithConfigProtection(step, &StepContext{Ctx: ctx, GateDir: gate, WorkDir: workDir}); err != nil {
			t.Fatal(err)
		}
	}
}
