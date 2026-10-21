package steps

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentCommitAttributionCreationAndAdoption(t *testing.T) {
	t.Parallel()
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepCI, types.StepRebase} {
		t.Run(string(step), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "submitted change\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
			head = gitCmd(t, dir, "rev-parse", "HEAD")
			ag := &mockAgent{name: "antigravity", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				writeAgentChange(t, dir)
				gitCmd(t, dir, "add", "-A")
				cmd := exec.CommandContext(ctx, testGitExecutable, "commit", "--no-verify", "-m", "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: no-mistakes antigravity <ai@example.com>\nAssisted-by: acp:cursor:model\nCodex-Session: abc\nOriginated-by: custom-agent\nCo-Authored-By: Claude Smith <human@example.com>")
				cmd.Dir, cmd.Env = dir, append(os.Environ(), opts.Env...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("agent commit: %v\n%s", err, out)
				}
				want := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <human@example.com>"
				if got := gitCmd(t, dir, "log", "-1", "--format=%B"); got != want {
					t.Fatalf("message at creation = %q, want %q", got, want)
				}
				return &agent.Result{}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Config.Commit.Trailers = []string{"Originated-by: custom-agent"}
			_, err := runPipelineCorrectionAgent(sctx, func() (*agent.Result, error) { return sctx.RunAgent(agent.RunOpts{CWD: dir}) })
			if err != nil {
				t.Fatal(err)
			}
			made := gitCmd(t, dir, "rev-parse", "HEAD")
			if step == types.StepCI {
				sctx.Config.CI.RevalidateRepairs = true
				if _, err := (&CIStep{}).commitRepair(sctx, "", nil); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := commitAgentFixes(sctx, step, "", "fallback"); err != nil {
					t.Fatal(err)
				}
			}
			if sctx.Run.HeadSHA != made {
				t.Fatalf("recorded head = %s, want %s", sctx.Run.HeadSHA, made)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD^"); got != head {
				t.Fatalf("submitted head changed: %s", got)
			}
		})
	}
}

func TestAgentCommitAttributionCleanWorktreeRefusesBypass(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
	gitCmd(t, dir, "commit", "--allow-empty", "-m", "repair widget\n\nAssisted-by: rovodev:model")
	attributed := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := commitAgentFixes(sctx, types.StepReview, "", "fallback"); err == nil {
		t.Fatal("adopted attributed clean-worktree head")
	}
	if sctx.Run.HeadSHA != head {
		t.Fatal("recorded attributed head")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != attributed {
		t.Fatal("rewrote existing commit")
	}
	if err := recordPipelineHeadRange(sctx, head, attributed); err == nil {
		t.Fatal("recorded bypassed agent commit")
	}
}

func TestAgentCommitAttributionBypassRejectedAtInvocation(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "commit", "--allow-empty", "-m", "repair widget\n\nCo-Authored-By: no-mistakes rovodev <ai@example.com>")
		return &agent.Result{}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir}); err == nil {
		t.Fatal("accepted agent invocation that bypassed filtering")
	}
	if sctx.Run.HeadSHA != head {
		t.Fatal("recorded bypassed head")
	}
}
