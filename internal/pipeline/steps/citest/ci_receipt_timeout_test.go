package citest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// Issue #87: settled green checks must not leave a missing comparison receipt
// as progress forever. The clock and forge boundary are controlled, not CI.
func receiptTimeoutFixture(t *testing.T, timeout, state string) (*pipeline.StepContext, func(string), *steps.CIStep, *time.Time) {
	t.Helper()
	dir, base, head := stepstest.SetupGitRepo(t)
	c := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, base, head, config.Commands{})
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "ci_timeout: unlimited\n"
	if timeout != "default" {
		yaml += "ci_readiness_receipt_timeout: " + timeout + "\n"
	}
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Config = config.Merge(global, &config.RepoConfig{})
	var statusesJSON atomic.Value
	statusesJSON.Store(receiptCheckJSON(head, state))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2.0/repositories/test/repo/pullrequests/42":
			fmt.Fprintf(w, `{"id":42,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/test/repo/pull-requests/42"}},"source":{"branch":{"name":"feature"},"repository":{"full_name":"test/repo"},"commit":{"hash":%q}},"destination":{"branch":{"name":"main"}}}`, head)
		case "/2.0/repositories/test/repo/pullrequests/42/statuses":
			fmt.Fprint(w, statusesJSON.Load().(string))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c.Env = []string{"NO_MISTAKES_BITBUCKET_EMAIL=test@example.com", "NO_MISTAKES_BITBUCKET_API_TOKEN=test-token", "NO_MISTAKES_BITBUCKET_API_BASE_URL=" + server.URL}
	setState := func(state string) { statusesJSON.Store(receiptCheckJSON(head, state)) }
	c.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	url := "https://bitbucket.org/test/repo/pull-requests/42"
	c.Run.PRURL = &url
	if err := c.DB.UpdateRunPRURL(c.Run.ID, url); err != nil {
		t.Fatal(err)
	}
	c.PRTarget = &pipeline.PRTargetSelection{PRURL: url, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	// Keep real Git comparison reads local, as in the neighbouring receipt tests.
	now := time.Unix(1700000000, 0)
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now })
	return c, setState, step, &now
}

func receiptCheckJSON(head, state string) string {
	return `{"values":[{"name":"build","state":"` + state + `","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/` + head + `"}}}]}`
}

func TestCIStepMissingReceiptTerminates(t *testing.T) {
	c, _, step, now := receiptTimeoutFixture(t, "default", "SUCCESSFUL")
	polls := 0
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls > 3 {
			return errors.New("test stopped unbounded monitor")
		}
		*now = now.Add(5 * time.Minute)
		return nil
	})
	_, err := step.Execute(c)
	if err == nil || !strings.Contains(err.Error(), "ci-readiness-receipt-missing") || !strings.Contains(err.Error(), c.Run.HeadSHA) || polls != 2 {
		t.Fatalf("missing receipt must fail by the default 10m bound with its name and head: polls=%d err=%v", polls, err)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.CIReadyAt != nil {
		t.Fatalf("missing receipt became ready: %+v %v", run, err)
	}
}

func TestCIStepReceiptAppearsBecomesReady(t *testing.T) {
	c, _, step, now := receiptTimeoutFixture(t, "2m", "SUCCESSFUL")
	stop := errors.New("test ended healthy monitor")
	polls := 0
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls == 1 {
			stepstest.BindCurrentPRComparison(t, c, *c.Run.PRURL, "test/repo", "main")
			*now = now.Add(time.Minute)
			return nil
		}
		run, err := c.DB.GetRun(c.Run.ID)
		if err != nil || run.CIReadyAt == nil {
			t.Fatalf("current receipt did not establish readiness: %+v %v", run, err)
		}
		// A healthy monitor stays alive well beyond the missing-receipt bound.
		if polls == 2 {
			*now = now.Add(time.Hour)
			return nil
		}
		return stop
	})
	_, err := step.Execute(c)
	if !errors.Is(err, stop) || polls != 3 {
		t.Fatalf("healthy receipt failed: polls=%d err=%v", polls, err)
	}
}

func TestCIStepReceiptBoundPreservesSlowChecks(t *testing.T) {
	c, setState, step, now := receiptTimeoutFixture(t, "2m", "INPROGRESS")
	polls := 0
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		switch polls {
		case 1:
			*now = now.Add(24 * time.Hour)
		case 2:
			// A day of running checks has spent none of the receipt budget.
			setState("SUCCESSFUL")
		case 3, 4:
			*now = now.Add(time.Minute)
		default:
			return errors.New("test stopped unbounded monitor")
		}
		return nil
	})
	_, err := step.Execute(c)
	if err == nil || !strings.Contains(err.Error(), "ci-readiness-receipt-missing") || polls != 4 {
		t.Fatalf("running checks spent the receipt budget, or missing receipt never stopped: polls=%d err=%v", polls, err)
	}
}

func TestCIStepReceiptBoundIsConfigurable(t *testing.T) {
	c, _, step, now := receiptTimeoutFixture(t, "1h", "SUCCESSFUL")
	polls := 0
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls > 3 {
			return errors.New("test stopped unbounded monitor")
		}
		*now = now.Add(30 * time.Minute)
		return nil
	})
	_, err := step.Execute(c)
	if err == nil || !strings.Contains(err.Error(), "ci-readiness-receipt-missing") || polls != 2 {
		t.Fatalf("configured 1h budget not honored: polls=%d err=%v", polls, err)
	}
}

func TestCIStepReceiptBoundResetsWhenChecksRunAgain(t *testing.T) {
	c, setState, step, now := receiptTimeoutFixture(t, "2m", "SUCCESSFUL")
	polls := 0
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		switch polls {
		case 1:
			*now = now.Add(time.Minute)
			setState("INPROGRESS")
		case 2:
			*now = now.Add(time.Hour)
			setState("SUCCESSFUL")
		case 3, 4:
			*now = now.Add(time.Minute)
		default:
			return errors.New("test stopped unbounded monitor")
		}
		return nil
	})
	_, err := step.Execute(c)
	if err == nil || !strings.Contains(err.Error(), "ci-readiness-receipt-missing") || polls != 4 {
		t.Fatalf("running-check observation failed to reset proof budget: polls=%d err=%v", polls, err)
	}
}
