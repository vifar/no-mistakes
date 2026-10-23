package steps

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
