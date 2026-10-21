package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredStructuralHookCreation(t *testing.T) {
	for _, noVerify := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "no-verify"}[noVerify], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			hooks := filepath.Join(t.TempDir(), "hooks")
			if err := os.Mkdir(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			const footer = "\n\nHook body edit.\nSigned-off-by: Hook Person <hook@example.com>\noRiGiNaTeD-bY :\tbespoke-bot\n"
			script := "#!/bin/sh\nprintf '" + footer + "' >> \"$1\"\nprintf 'ran\\n' >> \"$0.ran\"\n"
			for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
				if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, dir, "config", "core.hooksPath", hooks)
			ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				args := []string{"commit", "--allow-empty", "-m", "new correction"}
				if noVerify {
					args = append(args, "--no-verify")
				}
				cmd := exec.CommandContext(ctx, testGitExecutable, args...)
				cmd.Dir, cmd.Env = dir, append(os.Environ(), opts.Env...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("commit: %v\n%s", err, out)
				}
				want := "new correction\n\n\nHook body edit.\nSigned-off-by: Hook Person <hook@example.com>"
				wantTrailers := "Signed-off-by: Hook Person <hook@example.com>"
				if !noVerify {
					want += "\n\n\nHook body edit.\nSigned-off-by: Hook Person <hook@example.com>"
				}
				if got := string(storedReplayMessage(t, dir, "HEAD")); strings.Contains(got, "bespoke-bot") {
					t.Fatalf("hook-added attribution survived commit creation: %q", got)
				}
				if got := string(storedReplayMessage(t, dir, "HEAD")); got != want+"\n" {
					t.Fatalf("message before adoption = %q, want %q", got, want+"\n")
				}
				if got := gitTrailers(t, dir); got != wantTrailers {
					t.Fatalf("Git footer consumer = %q, want %q", got, wantTrailers)
				}
				return &agent.Result{}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.Commit.Trailers = []string{"Originated-by: bespoke-bot"}
			if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
				t.Fatal(err)
			}
			if err := commitAgentFixes(sctx, types.StepReview, "", "fallback"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
				data, err := os.ReadFile(filepath.Join(hooks, name+".ran"))
				if name == "commit-msg" && noVerify {
					if !os.IsNotExist(err) {
						t.Fatalf("--no-verify ran commit-msg: %q, %v", data, err)
					}
				} else if err != nil || string(data) != "ran\n" {
					t.Fatalf("%s side effect = %q, %v", name, data, err)
				}
			}
		})
	}
}

func TestConfiguredStructuralBypassRefused(t *testing.T) {
	for _, path := range []string{"fix", "rebase"} {
		t.Run(path, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
			sctx.Config.Commit.Trailers = []string{"Originated-by: bespoke-bot"}
			const message = "new correction\n\nHook body edit.\nSigned-off-by: Hook Person <hook@example.com>\nORIGINATED-BY:\tbespoke-bot"
			gitCmd(t, dir, "commit", "--allow-empty", "-m", message)
			bypass := gitCmd(t, dir, "rev-parse", "HEAD")
			original := storedReplayMessage(t, dir, "HEAD")
			if got := gitTrailers(t, dir); !strings.Contains(got, "bespoke-bot") {
				t.Fatalf("Git did not recognize bypass footer: %q", got)
			}
			var err error
			if path == "fix" {
				err = commitAgentFixes(sctx, types.StepReview, "", "fallback")
			} else {
				_, err = updateHeadSHA(sctx.Ctx, sctx, "main")
			}
			if err == nil || !strings.Contains(err.Error(), "AI attribution bypassed commit-time filtering") {
				t.Fatalf("adoption accepted bypass: %v", err)
			}
			stored, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || stored.HeadSHA != head || sctx.Run.HeadSHA != head {
				t.Fatalf("adopted bypass: %#v, %v", stored, err)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != bypass || !bytes.Equal(storedReplayMessage(t, dir, "HEAD"), original) {
				t.Fatal("validator changed the bypassed commit")
			}
		})
	}
}

func TestConfiguredStructuralRawSummary(t *testing.T) {
	for _, mode := range []string{"fix", "lint", "ci"} {
		t.Run(mode, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			raw, _ := json.Marshal(map[string]any{"summary": "repair widget\n\nORIGINATED-BY :\tbespoke-bot", "findings": []any{}, "code_change_needed": true, "verification": "passed"})
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
