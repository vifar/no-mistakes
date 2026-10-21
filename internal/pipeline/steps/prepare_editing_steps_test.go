//go:build !windows

package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Issue #77's oracle is an executable in the exact fix worktree. The fixture
// substitutes a tiny pinned runner for tsx; it executes a check on the fixer's
// edited file, rather than treating the agent's verification claim as proof.
func editingPreparation(t *testing.T, sctx *pipeline.StepContext) string {
	t.Helper()
	exclude := gitCmd(t, sctx.WorkDir, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(sctx.WorkDir, exclude)
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nnode_modules/\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	gate := t.TempDir()
	gitCmd(t, gate, "init", "--bare")
	sctx.GateDir = gate
	sctx.Shared = &pipeline.RunShared{}
	runner := filepath.Join(t.TempDir(), "tsx")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nset -eu\nif [ \"${1:-}\" = --version ]; then echo fixture-1; exit 0; fi\ntest \"$(cat feature.txt)\" = 'fixer edit'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("mkdir -p node_modules/.bin && cp %s node_modules/.bin/tsx && echo installed >> node_modules/installs && echo mutation > base.txt && echo stray > prepare-stray", shellQuotePreparation(runner))
}

func shellQuotePreparation(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func runPreparedCheck(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "node_modules", ".bin", "tsx"), args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verification blocked by missing tsx or failed check: %w: %s", err, out)
	}
	return nil
}

func TestEditingPreparation_ReviewFixHasDependencies(t *testing.T) {
	sctx, _, dir, _ := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if err := runPreparedCheck(context.Background(), dir, "--version"); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"summary":"verified existing fix","verification":"passed"}`), nil
	})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("review fixer in %s: %v", dir, err)
	}
}

func TestEditingPreparation_ReviewFixVerifiesItsEditWithoutSetupCommit(t *testing.T) {
	checked := false
	sctx, _, dir, head := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if err := editFeature(dir); err != nil {
			return nil, err
		}
		if err := runPreparedCheck(context.Background(), dir); err != nil {
			return nil, err
		}
		checked = true
		return json.RawMessage(`{"summary":"repair feature","verification":"passed"}`), nil
	})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("fixer's own check never passed")
	}
	if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("commits after fix = %s, want one real fix and no setup/format commit", got)
	}
	if got := gitCmd(t, dir, "diff", "--name-only", head, "HEAD"); got != "feature.txt" {
		t.Fatalf("setup artifacts committed: %q", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("setup artifacts left behind: %s", got)
	}
}

func TestEditingPreparation_TestStillInstallsAndProducesEvidence(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	evidence := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := runPreparedCheck(ctx, opts.CWD, "--version"); err != nil {
			return nil, err
		}
		evidence++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"checked","tested":["pinned runner"],"testing_summary":"runner executes","artifacts":[],"scenarios":[{"name":"run installed runner","result":"pass","live":true,"evidence":"fixture runner executed","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{Test: "node_modules/.bin/tsx --version"})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	out, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || evidence != 1 || !strings.Contains(out.Findings, `"verdict":"go"`) {
		t.Fatalf("Test evidence lost: %+v, calls=%d", out, evidence)
	}
}

func TestEditingPreparation_AgentLintHasDependencies(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := runPreparedCheck(ctx, opts.CWD, "--version"); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"lint verified"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("lint calls=%d", len(ag.calls))
	}
}

func TestEditingPreparation_SecondFixIsCheapAndReadOnlyReviewIsFree(t *testing.T) {
	sctx, _, dir, head := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if err := runPreparedCheck(context.Background(), dir, "--version"); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"summary":"verified no changes needed","verification":"passed"}`), nil
	})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	fixer := sctx.Agent
	sctx.Agent = &mockAgent{name: "review", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"ok","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`)}, nil
	}}
	sctx.Fixing = false
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("read-only review paid setup: %v", err)
	}
	sctx.Agent = fixer
	sctx.Fixing = true
	for i := 0; i < 2; i++ {
		// Model another round and recovery in this same worktree. Keep the external
		// agent's first-call fixer dispatch, as reviewFixContext expects.
		fixer.(*mockAgent).calls = nil
		sctx.Shared = &pipeline.RunShared{}
		if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "node_modules", "installs"))
	if err != nil || string(data) != "installed\n" {
		t.Fatalf("setup executions=%q error=%v, want exactly one", data, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head || sctx.Run.HeadSHA != head {
		t.Fatalf("setup-only rounds moved head: %s (run=%s), want %s", got, sctx.Run.HeadSHA, head)
	}
}

func TestEditingPreparation_RepositoryWithoutPrepareIsUnchanged(t *testing.T) {
	sctx, _, dir, head := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if _, err := os.Stat(filepath.Join(dir, "node_modules")); !os.IsNotExist(err) {
			return nil, fmt.Errorf("unexpected dependency setup: %v", err)
		}
		return json.RawMessage(`{"summary":"no check required","verification":"not_needed"}`), nil
	})
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("default moved head: %s", got)
	}
}

func TestEditingPreparation_FailureStopsBeforeFixer(t *testing.T) {
	sctx, ag, dir, head := reviewFixContext(t, func(string) (json.RawMessage, error) { t.Fatal("fixer ran after setup failure"); return nil, nil })
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx) + " && exit 23"
	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "prepare command exited with code 23") {
		t.Fatalf("error=%v, want explicit preparation failure", err)
	}
	if len(ag.calls) != 0 || gitCmd(t, dir, "rev-parse", "HEAD") != head || gitStatusPorcelain(t, dir) != "" {
		t.Fatal("failed setup entered fixer or changed tracked tree")
	}
	marker, err := preparationMarkerPath(sctx.Ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("failed setup recorded success: %v", err)
	}
}

func TestEditingPreparation_DocumentCombinedLintHasDependencies(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := runPreparedCheck(ctx, opts.CWD, "--version"); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"housekeeping verified"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.Commands.Prepare = editingPreparation(t, sctx)
	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 || ag.calls[0].Purpose != "housekeeping" {
		t.Fatalf("combined lint calls=%+v", ag.calls)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("setup moved document head: %s", got)
	}
}
