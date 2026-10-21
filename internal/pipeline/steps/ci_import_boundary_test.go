package steps

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIRepairImportsHeldTarget(t *testing.T) {
	for _, integration := range []string{"rebase", "merge"} {
		t.Run(integration, func(t *testing.T) {
			f := newMergeFixture(t, false)
			c := f.context(t, nil, integration)
			if _, err := tryRebase(c.Ctx, c, "origin/main"); err != nil {
				t.Fatal(err)
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "later.txt", "later upstream\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "upstream advance\n\nCodex-Session: imported")
			target := gitCmd(t, f.dir, "rev-parse", "HEAD")
			original := storedReplayMessage(t, f.dir, target)
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")
			c.Config.CI.RevalidateRepairs = true
			c.Agent = &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				run := func(args ...string) (string, error) { return git.Run(ctx, f.dir, args...) }
				if err := c.ValidateCommitAttribution(c.Run.HeadSHA, target, run); err != nil {
					t.Fatalf("held target not registered before agent: %v", err)
				}
				if !strings.Contains(opts.Prompt, "rebase target commit: "+target) {
					t.Fatal("prompt target differs")
				}
				if integration == "merge" {
					replayAgentGit(t, ctx, opts, "merge", "--no-edit", target)
				} else {
					replayAgentGit(t, ctx, opts, "rebase", target)
				}
				replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "--no-verify", "-m", "new correction\n\nCodex-Session: new")
				return &agent.Result{Output: []byte(`{"summary":"repair target","code_change_needed":true}`)}, nil
			}}
			if _, err := (&CIStep{}).autoFixCI(c, &forgejoLogTestHost{}, &scm.PR{Number: "42", BaseBranch: "main"}, ciFixTargets{BehindBase: true}); err != nil {
				t.Fatal(err)
			}
			if !isAncestor(c.Ctx, f.dir, target, "HEAD") {
				t.Fatal("target not integrated")
			}
			if !bytes.Equal(storedReplayMessage(t, f.dir, target), original) {
				t.Fatal("imported bytes changed")
			}
			if bytes.Contains(storedReplayMessage(t, f.dir, "HEAD"), []byte("Codex-Session:")) {
				t.Fatal("new correction not sanitized")
			}
			recorded := c.Run.HeadSHA
			gitCmd(t, f.dir, "-c", "core.hooksPath=", "commit", "--allow-empty", "-m", "bypass\n\nCodex-Session: new")
			bypass := gitCmd(t, f.dir, "rev-parse", "HEAD")
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err == nil {
				t.Fatal("bypass adopted")
			}
			stored, err := c.DB.GetRun(c.Run.ID)
			if err != nil || stored.HeadSHA != recorded || c.Run.HeadSHA != recorded || gitCmd(t, f.dir, "rev-parse", "HEAD") != bypass {
				t.Fatal("refusal changed heads", err)
			}
		})
	}
}
