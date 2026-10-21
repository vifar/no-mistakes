package steps

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the actual gh stderr boundary, not an opaque exit-status mock.
func TestIssue100StrictPRContextReadRetriesOnlyKeyringTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, detail       string
		failures, attempts int
		succeeds           bool
	}{
		{"recovers", "Timeout trying to log in to github.com account test (keyring)", 1, 2, true},
		{"bounded", "Timeout trying to log in to github.com account test (keyring)", 9, 3, false},
		{"not_logged_in", "You are not logged into any GitHub hosts", 9, 1, false},
		{"bad_credentials", "HTTP 401: Bad credentials", 9, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := stepstest.FakeCLIBinDir(t)
			stepstest.LinkFakeCLI(t, binDir, "gh")
			calls := 0
			var times []time.Time
			host := github.New(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				calls++
				times = append(times, time.Now())
				code := 0
				stdout, stderr := "main\n", ""
				if calls <= tc.failures {
					code = 1
					stdout = ""
					stderr = tc.detail
				}
				cmd := exec.CommandContext(ctx, filepath.Join(binDir, "gh"), args...)
				cmd.Env = stepstest.FakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "pr-context-read", "FAKE_CLI_STDOUT": stdout, "FAKE_CLI_STDERR": stderr, "FAKE_CLI_EXIT_CODE": fmt.Sprint(code)})
				return cmd
			}, nil, "", "")
			var logs []string
			sctx := &pipeline.StepContext{Ctx: context.Background(), Run: &db.Run{Shipping: &types.ShippingOptions{TargetBranch: "main"}}, Log: func(s string) { logs = append(logs, s) }}
			pr := &scm.PR{Number: "42"}
			err := verifyStrictPRBaseBranch(sctx.Ctx, sctx, host, pr)
			if (err == nil) != tc.succeeds || calls != tc.attempts {
				t.Fatalf("calls=%d error=%v; want %d attempts, succeeds=%t", calls, err, tc.attempts, tc.succeeds)
			}
			if tc.attempts > 1 {
				if times[1].Sub(times[0]) < time.Second {
					t.Fatalf("no real backoff: %v", times)
				}
				if !strings.Contains(strings.Join(logs, "\n"), "transient keyring timeout") {
					t.Fatalf("timeout absent from run log: %v", logs)
				}
			}
			if tc.succeeds && pr.BaseBranch != "main" {
				t.Fatalf("target=%q", pr.BaseBranch)
			}
		})
	}
}

// Real executor -> GuardPRContext -> gh auth status: a timeout at the boundary
// after Review must not replay Review or prevent Test from running.
func TestIssue100PostReviewAuthTimeoutContinuesTheSameRun(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sc.Run.PRURL = &prURL
	if err := sc.DB.UpdateRunPRURL(sc.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "timeout-once")
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","merged":false,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, prURL, head),
		"FAKE_CLI_AUTH_TIMEOUT_MARKER": marker,
	})
	reviewCalls, testCalls := 0, 0
	review := &transitionStep{name: types.StepReview, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		reviewCalls++
		if err := os.WriteFile(marker, []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		return &pipeline.StepOutcome{}, nil
	}}
	testStep := &transitionStep{name: types.StepTest, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		testCalls++
		return &pipeline.StepOutcome{}, nil
	}}
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ex := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{review, testStep}, nil)
	var logs []string
	ex.SetPRContextGuard(func(c *pipeline.StepContext, step types.StepName) (pipeline.PRContextDecision, error) {
		c.Env = env
		c.Log = func(s string) { logs = append(logs, s) }
		return GuardPRContext(c, step)
	})
	if err := ex.Execute(context.Background(), sc.Run, sc.Repo, dir); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 1 || testCalls != 1 {
		t.Fatalf("Review/Test calls=%d/%d", reviewCalls, testCalls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "transient keyring timeout") {
		t.Fatalf("logs=%v", logs)
	}
	run, err := sc.DB.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	rows, err := sc.DB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Status != types.StepStatusCompleted {
			t.Fatalf("lost validation: %+v", row)
		}
	}
}

func TestIssue100PublishedTargetRetentionRetriesReadTimeouts(t *testing.T) {
	for _, source := range []string{"protection", "rules", "mergeable"} {
		for _, tc := range []struct {
			name, detail       string
			failures, attempts int
		}{
			{"recovers", "Timeout trying to log in to github.com account test (keyring)", 1, 2},
			{"exhausts", "Timeout trying to log in to github.com account test (keyring)", 9, 3},
			{"credentials", "HTTP 401: Bad credentials", 9, 1},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				c, selection, receipt := retainedTargetFixture(t)
				gitCmd(t, c.WorkDir, "checkout", "main")
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "advance target")
				target := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "checkout", "feature")
				bin := fakeCLIBinDir(t)
				linkTestBinary(t, bin, "gh")
				calls := 0
				var starts []time.Time
				host := github.New(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
					operation, stdout := "", ""
					joined := strings.Join(args, " ")
					switch {
					case strings.Contains(joined, "/protection/required_status_checks"):
						operation, stdout = "protection", `{"strict":false}`
					case strings.Contains(joined, "/rules/branches/"):
						operation, stdout = "rules", `[[]]`
					case strings.Contains(joined, "mergeable,mergeStateStatus"):
						operation, stdout = "mergeable", "MERGEABLE CLEAN"
					default:
						t.Fatalf("unexpected read: %v", args)
					}
					code, stderr := 0, ""
					if operation == source {
						calls++
						starts = append(starts, time.Now())
						if calls <= tc.failures {
							code, stdout, stderr = 1, "", tc.detail
						}
					}
					cmd := exec.CommandContext(ctx, filepath.Join(bin, "gh"), args...)
					cmd.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "pr-context-read", "FAKE_CLI_STDOUT": stdout, "FAKE_CLI_STDERR": stderr, "FAKE_CLI_EXIT_CODE": fmt.Sprint(code)})
					return cmd
				}, nil, "github.com", "test/repo")
				var logs []string
				c.Log = func(s string) { logs = append(logs, s) }
				err := verifyCurrentPRComparison(c, host, selection, receipt, nil)
				if (err == nil) != (tc.name == "recovers") || calls != tc.attempts {
					t.Fatalf("calls=%d err=%v; want attempts=%d", calls, err, tc.attempts)
				}
				for i := 1; i < len(starts); i++ {
					if starts[i].Sub(starts[i-1]) < time.Duration(i)*time.Second {
						t.Fatalf("missing backoff: %v", starts)
					}
				}
				if tc.attempts > 1 && !strings.Contains(strings.Join(logs, "\n"), "transient keyring timeout") {
					t.Fatalf("missing transient log: %v", logs)
				}
				if tc.name == "exhausts" {
					if !pipeline.PRContextReadTimedOut(err.Error()) {
						t.Fatalf("lost timeout diagnostic: %v", err)
					}
					if e := c.DB.UpdateRunErrorStatus(c.Run.ID, err.Error(), types.RunFailed); e != nil {
						t.Fatal(e)
					}
					requests := postCommitStatus(t, c, db.CommitStatusPublication{SHA: receipt.LocalHeadSHA, Host: "github.com", Repo: "test/repo"}, "failure", false)
					if len(requests) != 1 || requests[0].State != "failure" || requests[0].Description != "PR-context unavailable: transient keyring timeout; validation incomplete" {
						t.Fatalf("status=%+v", requests)
					}
				}
				wantTarget := ""
				if err == nil {
					wantTarget = target
				}
				if got, e := c.DB.GetRetainedPRTarget(c.Run.ID, receipt); e != nil || got != wantTarget {
					t.Fatalf("retained target=%q want=%q err=%v", got, wantTarget, e)
				}
				if after, e := c.DB.GetRunPRContext(c.Run.ID); e != nil || *after != *receipt {
					t.Fatalf("comparison changed: %+v err=%v", after, e)
				}
				if run, e := c.DB.GetRun(c.Run.ID); e != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != receipt.LocalHeadSHA || run.HeadSHA != receipt.LocalHeadSHA {
					t.Fatalf("completed review or head changed: %+v err=%v", run, e)
				}
			})
		}
	}
}
