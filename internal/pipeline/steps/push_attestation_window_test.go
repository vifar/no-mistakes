package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTargetComparisonAdvancedNamesExactlyOneCondition proves the sentinel
// the push step keys on names ONE condition: the live target, merge base, or
// diff no longer matches the run's pinned receipt.
//
// The oracle is the comparison itself, not the error's shape: each case drives
// `verifyPRMutationComparison` with the stated live facts and asserts whether
// the sentinel came back. A changed PR identity, a changed head, a closed PR,
// and a dirty worktree are DIFFERENT conditions and must not claim to be a
// target advance.
func TestTargetComparisonAdvancedNamesExactlyOneCondition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(t *testing.T, sctx *pipeline.StepContext, host *mutationComparisonHost)
		wantTarget bool
	}{
		{name: "moved target", mutate: func(t *testing.T, sctx *pipeline.StepContext, _ *mutationComparisonHost) {
			gitCmd(t, sctx.WorkDir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(sctx.WorkDir, "advance.md"), []byte("target moved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, sctx.WorkDir, "add", "advance.md")
			gitCmd(t, sctx.WorkDir, "commit", "-m", "advance target")
			gitCmd(t, sctx.WorkDir, "checkout", "feature")
		}, wantTarget: true},
		{name: "changed PR identity", mutate: func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) {
			h.facts.BaseBranch = "develop"
		}},
		{name: "changed head", mutate: func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) {
			h.facts.HeadSHA = strings.Repeat("a", 40)
		}},
		{name: "closed PR", mutate: func(_ *testing.T, _ *pipeline.StepContext, h *mutationComparisonHost) {
			h.facts.State = "closed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, host, pr := mutationComparisonFixture(t)
			tc.mutate(t, sctx, host)
			err := verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
			if tc.wantTarget {
				if !errors.Is(err, errTargetComparisonAdvanced) {
					t.Fatalf("a moved target did not name the advance: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a changed comparison was accepted")
			}
			if errors.Is(err, errTargetComparisonAdvanced) {
				t.Fatalf("%s claims to be a target advance: %v", tc.name, err)
			}
		})
	}
}

// TestPushStep_RewindRequestPublishesNothing proves a push that takes the
// rewind publishes nothing and asks for exactly one restart.
func TestPushStep_RewindRequestPublishesNothing(t *testing.T) {
	c, _ := publicationFixture(t)
	advanceRewindTarget(t, c)
	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("the push produced no outcome")
	}
	if outcome.RestartFrom != types.StepRebase {
		t.Fatalf("the push asked for %q, want rebase", outcome.RestartFrom)
	}
	if outcome.NeedsApproval || outcome.Skipped || outcome.SkipRemaining {
		t.Fatalf("the rewind outcome also changed the gate state: %+v", outcome)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA != nil {
		t.Fatalf("the rewound push published: %+v", run.LastPushedSHA)
	}
}
