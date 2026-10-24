package citest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// ciCallBudgetFixture monitors a PR whose only check stays pending for an
// hour of simulated time and returns every gh invocation the monitor made and
// the waits between its polls.
//
// Measured on this fixture (issue: GitHub rate limits stalled ~10 lanes on
// 2026-10-24): 308 gh calls before (a 30/60/120s ladder, five `gh pr view`
// per poll), 67 after (unchanged polls back off to 5m; one combined view per
// poll plus the closing head re-read).
func ciCallBudgetFixture(t *testing.T) ([]string, []time.Duration) {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	pending := `[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`
	logPath := filepath.Join(t.TempDir(), "gh.log")
	env := append(stepstest.FakeCIGH(t, "OPEN", pending), "FAKE_CLI_LOG="+logPath, "FAKE_CLI_PR_HEAD_SHA="+headSHA)

	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	now := time.Unix(1700000000, 0)
	var waits []time.Duration
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).
		SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			now = now.Add(interval)
			return nil
		})
	if _, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls, waits
}

func TestCIStep_UnchangedPendingCIBacksOffItsPolling(t *testing.T) {
	t.Parallel()
	calls, waits := ciCallBudgetFixture(t)
	counts := map[string]int{}
	for _, call := range calls {
		fields := strings.Fields(call)
		counts[strings.Join(fields[:min(2, len(fields))], " ")]++
	}
	t.Logf("gh calls over one hour of unchanged pending CI: %d %v; polls: %d", len(calls), counts, len(waits)+1)
	if len(waits) == 0 || waits[0] != 30*time.Second {
		t.Fatalf("waits=%v, want the first wait on the normal 30s cadence", waits)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] && i != len(waits)-1 {
			t.Fatalf("waits=%v, want unchanged polls never to speed up", waits)
		}
		if waits[i] > 5*time.Minute {
			t.Fatalf("waits=%v, want the backoff capped at 5m", waits)
		}
	}
	if len(calls) > 80 {
		t.Fatalf("%d gh calls for one hour of unchanged CI, want at most 80 (308 before the backoff and the per-poll view)", len(calls))
	}
	polls := len(waits) + 1
	if counts["pr view"] > 2*polls+2 {
		t.Fatalf("%d gh pr view calls over %d polls, want one combined view plus the closing head re-read per poll", counts["pr view"], polls)
	}
}

func TestCIStep_ChangingChecksKeepTheNormalCadence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	sequence := []string{
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"IN_PROGRESS","bucket":"pending"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"lint","state":"IN_PROGRESS","bucket":"pending"},{"name":"test","state":"IN_PROGRESS","bucket":"pending"}]`,
	}
	env := append(stepstest.FakeCIGHSequence(t, "OPEN", sequence), "FAKE_CLI_PR_HEAD_SHA="+headSHA)
	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour

	now := time.Unix(1700000000, 0)
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).
		SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
			waits = append(waits, interval)
			now = now.Add(interval)
			if len(waits) == 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		})
	_, _ = stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	want := []time.Duration{30 * time.Second, 60 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits=%v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits=%v, want %v: one unchanged poll backs off, any change resets it", waits, want)
		}
	}
}
