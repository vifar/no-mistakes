package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A transport timeout at the gh boundary is a network failure, not a verdict on
// the work: the PR-context read must retry it and let the same run continue.
// Issue #200 measured this failing runs at the post-lint target check.
func TestTransientNetworkTimeoutContinuesThePRContextRead(t *testing.T) {
	binDir := stepstest.FakeCLIBinDir(t)
	stepstest.LinkFakeCLI(t, binDir, "gh")
	calls := 0
	host := github.New(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		calls++
		code, stdout, stderr := 0, "main\n", ""
		if calls == 1 {
			code, stdout = 1, ""
			stderr = `Post "https://api.github.com/graphql": dial tcp 140.82.112.5:443: i/o timeout`
		}
		cmd := exec.CommandContext(ctx, filepath.Join(binDir, "gh"), args...)
		cmd.Env = stepstest.FakeCLIEnv(binDir, map[string]string{
			"FAKE_CLI_MODE":      "pr-context-read",
			"FAKE_CLI_STDOUT":    stdout,
			"FAKE_CLI_STDERR":    stderr,
			"FAKE_CLI_EXIT_CODE": fmt.Sprint(code),
		})
		return cmd
	}, nil, "", "")
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{Shipping: &types.ShippingOptions{TargetBranch: "main"}},
		Log: func(string) {},
	}
	pr := &scm.PR{Number: "42"}
	start := time.Now()
	if err := verifyStrictPRBaseBranch(sctx.Ctx, sctx, host, pr); err != nil {
		t.Fatalf("a transient timeout stopped the run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("gh calls = %d, want the timed-out read then one retry", calls)
	}
	if pr.BaseBranch != "main" {
		t.Fatalf("target = %q, want the retried read's answer", pr.BaseBranch)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("retry returned in %v, want a real backoff", elapsed)
	}
}

func TestStepGitRetryClassificationUsesCommandDiagnostics(t *testing.T) {
	binDir := stepstest.FakeCLIBinDir(t)
	stepstest.LinkFakeCLI(t, binDir, "git")
	for _, verb := range []string{"fetch", "ls-remote", "push"} {
		t.Run(verb, func(t *testing.T) {
			sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: t.TempDir(), Env: stepstest.FakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE": "pr-context-read", "FAKE_CLI_STDERR": "fatal: unable to access 'https://github.com/test/conflict.git/': Operation timed out", "FAKE_CLI_EXIT_CODE": "128",
			})}
			_, err := stepGitRunRaw(sctx, verb, "origin", "refs/heads/fix/non-fast-forward-conflict-resolution")
			if class := netretry.Describe(err); class != string(netretry.ClassTimeout) {
				t.Fatalf("class=%q err=%v, want a retryable timeout despite argument text", class, err)
			}
		})
	}
}

func TestCommitStatusRemoteSHARetriesOnlyTransientFailures(t *testing.T) {
	for _, scenario := range []string{"transient", "auth", "cancelled backoff"} {
		t.Run(scenario, func(t *testing.T) {
			bin := stepstest.FakeCLIBinDir(t)
			stepstest.LinkFakeCLI(t, bin, "git")
			root := t.TempDir()
			trace := filepath.Join(root, "calls")
			marker := filepath.Join(root, "first-failure")
			if err := os.WriteFile(marker, []byte("fail once"), 0600); err != nil {
				t.Fatal(err)
			}
			vars := map[string]string{
				"FAKE_CLI_MODE":                   "pr-context-read",
				"FAKE_CLI_LOG":                    trace,
				"FAKE_CLI_STDOUT":                 "abc123\trefs/heads/feature\n",
				"FAKE_CLI_TRANSIENT_FAILURE_FILE": marker,
				"FAKE_CLI_TRANSIENT_FAILURE":      "fatal: unable to access 'https://github.com/test/repo.git': Operation timed out",
			}
			if scenario == "auth" {
				delete(vars, "FAKE_CLI_TRANSIENT_FAILURE_FILE")
				vars["FAKE_CLI_STDERR"] = "fatal: Authentication failed"
				vars["FAKE_CLI_EXIT_CODE"] = "128"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sc := &pipeline.StepContext{Ctx: ctx, WorkDir: root, Env: stepstest.FakeCLIEnv(bin, vars), Log: func(string) {
				if scenario == "cancelled backoff" {
					cancel()
				}
			}}
			sha, err := commitStatusRemoteSHA(sc, "https://github.com/test/repo.git", "refs/heads/feature")
			raw, readErr := os.ReadFile(trace)
			if readErr != nil {
				t.Fatal(readErr)
			}
			calls := len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
			if scenario == "transient" {
				if err != nil || sha != "abc123" || calls != 2 {
					t.Fatalf("sha=%q calls=%d err=%v, want transient recovery", sha, calls, err)
				}
			} else {
				if err == nil || calls != 1 {
					t.Fatalf("sha=%q calls=%d err=%v, want failure without retry", sha, calls, err)
				}
				if scenario == "cancelled backoff" && !errors.Is(err, context.Canceled) {
					t.Fatalf("backoff cancellation was lost: %v", err)
				}
			}
		})
	}
}
