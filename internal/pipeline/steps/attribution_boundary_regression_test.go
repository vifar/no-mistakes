package steps

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAttributionBoundaryRawConfiguredSummary(t *testing.T) {
	for _, mode := range []string{"fix", "lint", "ci"} {
		t.Run(mode, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			raw, _ := json.Marshal(map[string]any{"summary": "repair widget\n\nOriginated-by: bespoke-bot", "findings": []any{}, "code_change_needed": true, "verification": "passed"})
			ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				writeAgentChange(t, dir)
				return &agent.Result{Output: raw}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.Commit.Trailers = []string{"Originated-by: bespoke-bot"}
			step := types.StepReview
			switch mode {
			case "fix":
				sctx.Fixing = true
				if _, err := executeFixMode(sctx, step, fixExecutionOptions{RequireVerification: true}); err != nil {
					t.Fatal(err)
				}
			case "lint":
				step = types.StepLint
				if _, err := (&LintStep{}).Execute(sctx); err != nil {
					t.Fatal(err)
				}
			case "ci":
				step = types.StepCI
				sctx.Config.CI.RevalidateRepairs = true
				if _, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}, ciTargetsFor([]string{"test"}, false)); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := lastCommitMessage(t, dir), "no-mistakes("+string(step)+"): repair widget"; got != want {
				t.Fatalf("stored subject = %q, want %q", got, want)
			}
		})
	}
}

func TestAttributionBoundaryPersistentProcess(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "side-effects")
	script := "#!/bin/sh\nprintf 'ran\\n' >> '" + marker + "'\nprintf '\\nCo-Authored-By: Codex <model@example.com>\\n' >> \"$1\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "core.hooksPath", hooks)
	var process *exec.Cmd
	var input io.WriteCloser
	var output *bufio.Reader
	var turn int
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if process == nil {
			process = exec.Command("/bin/sh")
			process.Dir, process.Env = dir, append(os.Environ(), opts.Env...)
			var err error
			input, err = process.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			pipe, err := process.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			process.Stderr = os.Stderr
			output = bufio.NewReader(pipe)
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
		}
		turn++
		bot := []string{"first-bot", "second-bot"}[turn-1]
		fmt.Fprintf(input, "'%s' commit --allow-empty -m 'repair widget\n\nOriginated-by: %s' >/dev/null; printf 'done\\n'\n", testGitExecutable, bot)
		if line, err := output.ReadString('\n'); err != nil || line != "done\n" {
			t.Fatalf("persistent turn: %q, %v", line, err)
		}
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	defer func() {
		if input != nil {
			input.Close()
			if err := process.Wait(); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, bot := range []string{"first-bot", "second-bot"} {
		sctx.Config.Commit.Trailers = []string{"Originated-by: " + bot}
		if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
			t.Fatal(err)
		}
		if got := gitCmd(t, dir, "log", "-1", "--format=%B"); got != "repair widget" {
			t.Fatalf("persistent commit = %q", got)
		}
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "ran\nran\n" {
		t.Fatalf("repository hook effects = %q, %v", got, err)
	}
}

func attributionAppendingHook(t *testing.T, dir string) {
	t.Helper()
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\nprintf '\\n\\nCodex-Session: hook-added\\nSigned-off-by: Keeper <keeper@example.com>\\n' >> \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	rewriteScript := "#!/bin/sh\ncat >> '" + filepath.Join(hooks, "rewrite-input") + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-rewrite"), []byte(rewriteScript), 0700); err != nil {
		t.Fatal(err)
	}
	sourceLog := filepath.Join(hooks, "source-log")
	prepare := "#!/bin/sh\nprintf '%s %s [%s]\\n' \"$2\" \"$3\" \"$GIT_REFLOG_ACTION\" >> '" + sourceLog + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(prepare), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(sourceLog); err == nil {
			t.Logf("Git prepare-commit-msg source protocol: %s", data)
		}
	})
	gitCmd(t, dir, "config", "core.hooksPath", hooks)
}

func TestAttributionBoundaryAutomaticMerges(t *testing.T) {
	for _, path := range []string{"try", "agent"} {
		t.Run(path, func(t *testing.T) {
			f := newMergeFixture(t, false)
			attributionAppendingHook(t, f.dir)
			sctx := f.context(t, &mockAgent{}, "merge")
			if path == "try" {
				if _, err := tryMerge(sctx.Ctx, sctx, "origin/main"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := mergeWithAgent(sctx.Ctx, sctx, "origin/main"); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitTrailers(t, f.dir); got != "Signed-off-by: Keeper <keeper@example.com>" {
				t.Fatalf("merge trailers = %q", got)
			}
			if got := parents(t, f.dir, "HEAD"); len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainSHA {
				t.Fatalf("merge parents = %v", got)
			}
		})
	}
}

func TestAttributionBoundaryRebaseAdoption(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "new correction\n\nCodex-Session: bypass")
	if _, err := updateHeadSHA(sctx.Ctx, sctx, "main"); err == nil {
		t.Fatal("adopted attributed new commit")
	}
	if sctx.Run.HeadSHA != head {
		t.Fatal("changed in-memory head before validation")
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || stored.HeadSHA != head {
		t.Fatalf("recorded head = %#v, %v", stored, err)
	}
}

func TestAttributionBoundarySubmittedRebaseReplay(t *testing.T) {
	f := newMergeFixture(t, true)
	const message = "submitted feature\n\nKeep the submitted body.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted"
	gitCmd(t, f.dir, "commit", "--amend", "-m", message)
	f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
	attributionAppendingHook(t, f.dir)
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeFixtureFile(t, f.dir, "shared.txt", "feature line\nmain line\n")
		gitCmd(t, f.dir, "add", "-A")
		run := func(args ...string) {
			cmd := exec.CommandContext(ctx, testGitExecutable, args...)
			cmd.Dir = f.dir
			cmd.Env = append(append(os.Environ(), opts.Env...), "GIT_EDITOR=true")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("agent git %v: %v\n%s", args, err, out)
			}
		}
		run("commit", "--allow-empty", "--only", "-m", "new correction before continue\n\nCodex-Session: fresh")
		if got := gitTrailers(t, f.dir); got != "Signed-off-by: Keeper <keeper@example.com>" {
			t.Fatalf("active-rebase correction trailers = %q", got)
		}
		run("rebase", "--continue")
		got := gitCmd(t, f.dir, "log", "-1", "--format=%B")
		if !strings.HasPrefix(got, message) || strings.Contains(got, "hook-added") {
			t.Fatalf("submitted replay message = %q", got)
		}
		run("commit", "--allow-empty", "-m", "new correction\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: fresh")
		if got := gitTrailers(t, f.dir); got != "Signed-off-by: Keeper <keeper@example.com>" {
			t.Fatalf("new correction trailers = %q", got)
		}
		run("commit", "--allow-empty", "-m", message)
		if got := gitTrailers(t, f.dir); got != "Signed-off-by: Keeper <keeper@example.com>" {
			t.Fatalf("new correction reusing submitted text = %q", got)
		}
		return &agent.Result{}, nil
	}}
	sctx := f.context(t, ag, "rebase")
	sctx.Run.SubmittedHeadSHA = &f.headSHA
	if err := rebaseWithAgent(sctx.Ctx, sctx, "origin/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := updateHeadSHA(sctx.Ctx, sctx, "main"); err != nil {
		t.Fatal(err)
	}
	originalHooks := gitCmd(t, f.dir, "config", "core.hooksPath")
	rewriteInput, err := os.ReadFile(filepath.Join(originalHooks, "rewrite-input"))
	if err != nil || !strings.Contains(string(rewriteInput), f.headSHA+" ") {
		t.Fatalf("original post-rewrite input = %q, %v", rewriteInput, err)
	}
	if got := gitCmd(t, f.dir, "show", "-s", "--format=%B", f.headSHA); got != message {
		t.Fatalf("submitted object changed: %q", got)
	}
}

func TestAttributionBoundaryPersistedReviewOutcome(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepDocument, types.StepLint} {
		t.Run(string(step), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
			writeAgentChange(t, dir)
			if err := commitAgentFixes(sctx, step, "guard unsafe value", "fallback"); err != nil {
				t.Fatal(err)
			}
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, step)
			if err != nil {
				t.Fatal(err)
			}
			finding := `{"findings":[{"id":"f1","severity":"error","description":"unsafe value"}]}`
			if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", &finding, nil, 0); err != nil {
				t.Fatal(err)
			}
			summary := "guard unsafe value"
			if step != types.StepReview {
				summary = changesAppliedSummary
			}
			if _, err := sctx.DB.InsertReviewStepRound(sr.ID, 2, "auto_fix", nil, &summary, sctx.Run.HeadSHA, 0, sctx.FixApplied); err != nil {
				t.Fatal(err)
			}
			sr.Status = types.StepStatusCompleted
			rounds, err := sctx.DB.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			md, _ := BuildPipelineSummary([]*db.StepResult{sr}, map[string][]*db.StepRound{sr.ID: rounds}, sctx.Run.HeadSHA)
			if !strings.Contains(md, "auto-fixed") || !strings.Contains(md, "🔧 Fix applied.") || strings.Contains(md, "result not reported") {
				t.Fatalf("persisted fix rendering:\n%s", md)
			}
		})
	}
}
