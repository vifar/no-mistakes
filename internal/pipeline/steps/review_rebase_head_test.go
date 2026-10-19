package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewAfterBaseMergeRefreshesStaleRunHead(t *testing.T) {
	for _, tc := range []struct {
		name              string
		staleDatabase     bool
		finalizingAnswers bool
	}{
		{name: "durable post-merge head"},
		{name: "unproven post-merge head refuses before analyzer", staleDatabase: true},
		{name: "stale answered review cannot move to the proven merge head", finalizingAnswers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeFixture(t, false)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"clean fixture","risk_scope":"source-or-external"}`)}, nil
			}}
			c := f.context(t, ag, config.RebaseStrategyMerge)
			before := *c.Run
			initial, err := prComparisonAtTarget(c, "main", f.mainSHA, f.headSHA)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.DB.BindRunPRContext(c.Run.ID, initial, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			out, err := (&RebaseStep{}).Execute(c)
			if err != nil || out.NeedsApproval {
				t.Fatalf("base merge: %+v %v", out, err)
			}
			merged := gitCmd(t, f.dir, "rev-parse", "HEAD")
			ps := parents(t, f.dir, merged)
			if len(ps) != 2 || ps[0] != f.headSHA || ps[1] != f.mainSHA {
				t.Fatalf("not the reported merge shape: %v", ps)
			}
			comparison, err := prComparisonAtTarget(c, "main", f.mainSHA, merged)
			if err != nil {
				t.Fatal(err)
			}
			if bound, err := c.DB.BindRunPRContext(c.Run.ID, comparison, types.StepReview); err != nil || bound.Generation != 2 {
				t.Fatalf("post-rebase pin: %+v %v", bound, err)
			}
			c.PRContext, err = c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.PRTarget = &pipeline.PRTargetSelection{TargetBranch: "main"}
			// Reproduce the observed stale execution snapshot while Git and the
			// persisted comparison already name the post-rebase merge.
			c.Run = &before
			c.FinalizingAnswers = tc.finalizingAnswers
			if tc.staleDatabase {
				if err := c.DB.UpdateRunHeadSHA(c.Run.ID, before.HeadSHA); err != nil {
					t.Fatal(err)
				}
			}
			out, err = (&ReviewStep{}).Execute(c)
			if tc.staleDatabase || tc.finalizingAnswers {
				var precondition *ReviewHeadPreconditionError
				if !errors.As(err, &precondition) || len(ag.calls) != 0 || !strings.Contains(err.Error(), before.HeadSHA) || !strings.Contains(err.Error(), merged) {
					t.Fatalf("mismatch must refuse before analyzer with both heads: out=%+v err=%v calls=%d", out, err, len(ag.calls))
				}
				return
			}
			if err != nil || out == nil || out.ReviewApprovedHeadSHA != merged || c.Run.HeadSHA != merged || len(ag.calls) != 1 {
				t.Fatalf("review must bind post-rebase head once: out=%+v err=%v run=%s merged=%s calls=%d", out, err, c.Run.HeadSHA, merged, len(ag.calls))
			}
			stored, err := c.DB.GetRun(c.Run.ID)
			if err != nil || stored.HeadSHA != merged || stored.ReviewApprovedHeadSHA != nil {
				t.Fatalf("head refresh changed durable approval authority: %+v %v", stored, err)
			}
		})
	}
}
