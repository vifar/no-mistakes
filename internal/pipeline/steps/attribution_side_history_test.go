package steps

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAttributionSideHistoryAdoption(t *testing.T) {
	for _, path := range []string{"agent", "range", "fix", "exec", "integration"} {
		t.Run(path, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			var bad, merged string
			var message []byte
			bypass := func() {
				gitCmd(t, dir, "checkout", "-b", "new-side")
				gitCmd(t, dir, "-c", "core.hooksPath=", "commit", "--allow-empty", "-m", "side correction\n\nCodex-Session: bypass")
				bad = gitCmd(t, dir, "rev-parse", "HEAD")
				message = storedReplayMessage(t, dir, bad)
				gitCmd(t, dir, "checkout", "feature")
				gitCmd(t, dir, "-c", "core.hooksPath=", "merge", "--no-ff", "new-side", "-m", "clean integration")
				merged = gitCmd(t, dir, "rev-parse", "HEAD")
			}
			ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) { bypass(); return &agent.Result{}, nil }}
			c := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			var err error
			if path == "agent" {
				_, err = c.RunAgent(agent.RunOpts{CWD: dir})
			} else {
				bypass()
				switch path {
				case "range":
					err = recordPipelineHeadRange(c, head, merged)
				case "fix":
					err = commitAgentFixes(c, types.StepReview, "", "fallback")
				case "exec":
					_, err = stepGitHeadSHA(c)
				case "integration":
					_, err = updateHeadSHA(c.Ctx, c, "main")
				}
			}
			if err == nil || !strings.Contains(err.Error(), "AI attribution bypassed commit-time filtering") {
				t.Fatalf("adopted attributed side history: %v", err)
			}
			stored, e := c.DB.GetRun(c.Run.ID)
			if e != nil || stored.HeadSHA != head || c.Run.HeadSHA != head {
				t.Fatalf("recorded bypass: %#v, %v", stored, e)
			}
			if gitCmd(t, dir, "rev-parse", "HEAD") != merged || !bytes.Equal(storedReplayMessage(t, dir, bad), message) {
				t.Fatal("validator rewrote bypass")
			}
		})
	}
}

func TestAttributionProtectedSideHistory(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(map[bool]string{false: "submitted", true: "target"}[imported], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "-b", "old-side")
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "old side history\n\nCodex-Session: original")
			side := gitCmd(t, dir, "rev-parse", "HEAD")
			message := storedReplayMessage(t, dir, side)
			gitCmd(t, dir, "checkout", "feature")
			submitted := head
			if !imported {
				gitCmd(t, dir, "merge", "--no-ff", "old-side", "-m", "submitted merge")
				submitted = gitCmd(t, dir, "rev-parse", "HEAD")
			}
			c := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, submitted, config.Commands{})
			run := func(args ...string) (string, error) { return git.Run(c.Ctx, dir, args...) }
			var targets []string
			if imported {
				targets = []string{side}
			}
			if _, err := c.CommitAttributionEnv(nil, run, targets...); err != nil {
				t.Fatal(err)
			}
			if imported {
				gitCmd(t, dir, "merge", "--no-ff", "old-side", "-m", "actual target integration")
			} else {
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "new clean correction")
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(storedReplayMessage(t, dir, side), message) {
				t.Fatal("protected side object changed")
			}
		})
	}
}
