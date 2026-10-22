package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIssue131FailsTheStepThatChangesSharedConfig(t *testing.T) {
	testSharedConfigWriterFails(t, false, false)
}

func TestIssue131SharedConfigViolationCannotBecomeProtectedPathApproval(t *testing.T) {
	for _, fixing := range []bool{false, true} {
		name := "initial"
		if fixing {
			name = "fix-round"
		}
		t.Run(name, func(t *testing.T) { testSharedConfigWriterFails(t, true, fixing) })
	}
}

func testSharedConfigWriterFails(t *testing.T, protected, fixing bool) {
	database, p, run, repo := setupTest(t)
	seed := completionFixture(t, database, run)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	var cfg *config.Config
	if fixing {
		cfg = &config.Config{AutoFix: config.AutoFix{Review: 1}}
	}
	executor := NewExecutor(database, p, cfg, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if fixing && !sctx.Fixing {
				return &StepOutcome{AutoFixable: true, Findings: `{"findings":[{"id":"repair","severity":"error","description":"repair needed","action":"auto-fix"}]}`}, nil
			}
			if protected {
				if err := os.WriteFile(filepath.Join(wt, "package.lock"), []byte("protected repair\n"), 0o600); err != nil {
					return nil, err
				}
			}
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
			if closeErr != nil {
				return nil, closeErr
			}
			if protected {
				return nil, fmt.Errorf("stage repair: %w", &ProtectedPathError{Path: "package.lock", Rule: "*.lock"})
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) { later = true; return &StepOutcome{}, nil }},
	}, nil)
	err := executor.Execute(ctx, run, repo, wt)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "core.bare") {
		t.Fatalf("want shared file/key failure, got %v", err)
	}
	stored, dbErr := database.GetRun(run.ID)
	if dbErr != nil || stored == nil || stored.Status != types.RunFailed {
		t.Fatalf("writer run did not fail: %+v, %v", stored, dbErr)
	}
	if protected {
		if !strings.Contains(err.Error(), "dirty protected path") {
			t.Fatalf("lost protected-path error: %v", err)
		}
		if data, readErr := os.ReadFile(filepath.Join(wt, "package.lock")); readErr != nil || string(data) != "protected repair\n" {
			t.Fatalf("protected edit lost: %q, %v", data, readErr)
		}
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
			_, err := executeWithConfigProtection(step.Name(), &StepContext{Ctx: context.Background(), GateDir: gate}, step.Execute)
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
		if _, err := executeWithConfigProtection(step.Name(), &StepContext{Ctx: ctx, GateDir: gate, WorkDir: workDir}, step.Execute); err != nil {
			t.Fatal(err)
		}
	}
}
