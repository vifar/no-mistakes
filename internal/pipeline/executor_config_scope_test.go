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

func TestIssue131GuardAllowsSubmodulePreparationAndRestoration(t *testing.T) {
	ctx := context.Background()
	command := func(dir string, args ...string) string {
		t.Helper()
		out, err := git.Run(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := t.TempDir()
	module := filepath.Join(base, "module")
	command(base, "init", module)
	command(module, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "module")
	inner := filepath.Join(base, "inner")
	command(base, "init", inner)
	command(inner, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "nested module")
	command(module, "-c", "protocol.file.allow=always", "submodule", "add", inner, "nested")
	command(module, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-am", "nested dependency")
	seed := filepath.Join(base, "seed")
	command(base, "init", seed)
	command(seed, "-c", "protocol.file.allow=always", "submodule", "add", module, "dependency")
	command(seed, "config", "--file", ".gitmodules", "submodule.dependency.url", "../module")
	command(seed, "config", "--file", ".gitmodules", "submodule.dependency.update", "checkout")
	command(seed, "add", ".gitmodules", "dependency")
	command(seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "superproject")
	bare := filepath.Join(base, "gate.git")
	command(base, "clone", "--bare", seed, bare)
	if err := git.IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := git.RecordSharedConfigBaseline(ctx, bare); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test", "lint"} {
		wt := filepath.Join(base, name)
		if err := git.WorktreeAdd(ctx, bare, wt, "HEAD"); err != nil {
			t.Fatal(err)
		}
		step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := git.Run(ctx, wt, "config", "forbidden.shared", "write"); err == nil {
				t.Fatal("shared config lock was relaxed")
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := git.Run(ctx, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive"); err != nil {
					return nil, err
				}
				if got := command(filepath.Join(wt, "dependency"), "rev-parse", "HEAD"); got != command(module, "rev-parse", "HEAD") {
					t.Fatalf("prepared checkout = %s", got)
				}
				if got := command(filepath.Join(wt, "dependency", "nested"), "rev-parse", "HEAD"); got != command(inner, "rev-parse", "HEAD") {
					t.Fatalf("nested prepared checkout = %s", got)
				}
				if err := os.RemoveAll(filepath.Join(wt, "dependency")); err != nil {
					return nil, err
				}
				if _, err := git.Run(ctx, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive", "--no-fetch", "--checkout", "--force", "--", "dependency"); err != nil {
					return nil, err
				}
			}
			return &StepOutcome{}, nil
		}}
		if _, err := executeWithConfigProtection(step, &StepContext{Ctx: ctx, GateDir: bare, WorkDir: wt}); err != nil {
			t.Fatalf("%s preparation: %v", name, err)
		}
		for key, want := range map[string]string{"submodule.dependency.url": module, "submodule.dependency.update": "checkout"} {
			if got := command(wt, "config", "--worktree", "--get", key); got != want {
				t.Fatalf("worktree %s = %q, want %q", key, got, want)
			}
		}
	}
	after, err := os.ReadFile(filepath.Join(bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("submodule preparation changed shared config")
	}
}
