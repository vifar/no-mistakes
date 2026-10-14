package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestCIStepNonCurrentChecksNeverDispatchRepair(t *testing.T) {
	for _, state := range []string{"FAILED", "STOPPED", "SUCCESSFUL"} {
		for _, evidence := range []string{"stale", "missing"} {
			t.Run(state+"/"+evidence, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				link := ""
				if evidence == "stale" {
					link = fmt.Sprintf(`,"links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}`, base)
				}
				api := newFakeBitbucketCIAPI(t, "OPEN", fmt.Sprintf(`{"values":[{"key":"test","name":"test","state":%q%s}]}`, state, link), head)
				ag := &mockAgent{name: "test"}
				c := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				prURL := "https://bitbucket.org/test/repo/pull-requests/42"
				c.Run.PRURL = &prURL
				c.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
				c.Env = fakeBitbucketEnv(api.server.URL)
				c.Config.CITimeout = -1
				c.Config.AutoFix.CI = 1
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.Ctx = ctx
				polls := 0
				ci := (&CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
					polls++
					cancel()
					return ctx.Err()
				})
				outcome, err := driveCI(t, ci, c)
				if !errors.Is(err, context.Canceled) || polls != 1 || outcome != nil || len(ag.calls) != 0 {
					t.Fatalf("non-current checks triggered work: polls=%d outcome=%+v err=%v fixerCalls=%d", polls, outcome, err, len(ag.calls))
				}
				run, err := c.DB.GetRun(c.Run.ID)
				if err != nil || run.CIReadyAt != nil {
					t.Fatalf("non-current checks made CI ready: run=%+v err=%v", run, err)
				}
				c.Ctx = context.Background()
				reason, err := ci.VerifyApprovalOverride(c)
				if err != nil || !strings.Contains(reason, "not for the commit being delivered") {
					t.Fatalf("approval used non-current verdict: reason=%q err=%v", reason, err)
				}
			})
		}
	}
}

func TestCIStepCurrentFailedChecksRemainActionable(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", fmt.Sprintf(`{"values":[{"key":"test","name":"test","state":"FAILED","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}}]}`, head), head)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	c.Run.PRURL = &prURL
	c.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	c.Env = fakeBitbucketEnv(api.server.URL)
	c.Config.CITimeout = -1
	ci := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		t.Fatal("current failed check should produce findings")
		return nil
	})
	outcome, err := ci.Execute(c)
	if err != nil || outcome == nil || !strings.Contains(outcome.Findings, `"check":"test"`) {
		t.Fatalf("current failure lost: outcome=%+v err=%v", outcome, err)
	}
}
