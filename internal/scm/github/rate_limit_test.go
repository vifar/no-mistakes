package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The messages gh printed on 2026-10-24 for the runs that failed review and
// test work: the GraphQL primary limit, and its REST counterpart.
const (
	ghGraphQLRateLimit = "GraphQL: API rate limit already exceeded for user ID 12345."
	ghRESTRateLimit    = "gh: API rate limit exceeded for user ID 12345. (HTTP 403)"
	ghSecondaryLimit   = "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)"
)

type rateLimitScript struct {
	mu    sync.Mutex
	args  [][]string
	steps []ghScriptStep
}

func (s *rateLimitScript) factory(t *testing.T) CmdFactory {
	inner, _ := sequencedGHFactory(t, s.steps)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		s.mu.Lock()
		s.args = append(s.args, append([]string(nil), args...))
		s.mu.Unlock()
		return inner(ctx, name, args...)
	}
}

func rateLimitHost(t *testing.T, script *rateLimitScript, now time.Time) (*Host, *[]time.Duration) {
	host := fastRetryHost(script.factory(t))
	waits := &[]time.Duration{}
	clock := now
	host.rateLimitNow = func() time.Time { return clock }
	host.rateLimitWait = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		clock = clock.Add(d)
		return nil
	}
	return host, waits
}

func totalWait(waits []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range waits {
		total += d
	}
	return total
}

func TestPrimaryGraphQLRateLimitWaitsForTheReportedReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(17 * time.Minute)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghGraphQLRateLimit, code: 1},
		{stdout: fmt.Sprintf(`{"resources":{"core":{"remaining":4000,"reset":%d},"graphql":{"remaining":0,"reset":%d}}}`, now.Unix(), reset.Unix())},
		{stdout: "dev\n"},
	}}
	host, waits := rateLimitHost(t, script, now)
	base, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"})
	if err != nil || base != "dev" {
		t.Fatalf("base=%q err=%v, want the read to succeed after the reset", base, err)
	}
	if got := strings.Join(script.args[1], " "); got != "api -X GET rate_limit" {
		t.Fatalf("second call = %q, want the rate_limit probe", got)
	}
	if total := totalWait(*waits); total < 17*time.Minute || total > 17*time.Minute+30*time.Second {
		t.Fatalf("waited %v, want the GraphQL reset (17m) plus spread", total)
	}
}

func TestPrimaryMessageWithBudgetLeftIsTheSecondaryLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghGraphQLRateLimit, code: 1},
		// 14:33Z on 2026-10-24: rateLimit.remaining was 3954 while gh pr view
		// still failed with the primary-limit text.
		{stdout: fmt.Sprintf(`{"resources":{"graphql":{"remaining":3954,"reset":%d}}}`, now.Add(40*time.Minute).Unix())},
		{stdout: "OPEN\n"},
	}}
	host, waits := rateLimitHost(t, script, now)
	state, err := host.GetPRState(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"})
	if err != nil || state != scm.PRStateOpen {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if total := totalWait(*waits); total < time.Minute || total > 72*time.Second {
		t.Fatalf("waited %v, want a secondary-limit backoff of about 60s, not the 40m reset", total)
	}
}

func TestRESTPrimaryLimitReadsTheCoreReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghRESTRateLimit, code: 1},
		{stdout: fmt.Sprintf(`{"resources":{"core":{"remaining":0,"reset":%d},"graphql":{"remaining":5000,"reset":%d}}}`, now.Add(5*time.Minute).Unix(), now.Unix())},
		{stdout: "ok"},
	}}
	host, waits := rateLimitHost(t, script, now)
	if _, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if total := totalWait(*waits); total < 5*time.Minute || total > 5*time.Minute+30*time.Second {
		t.Fatalf("waited %v, want the core reset", total)
	}
}

func TestPrimaryLimitProbeFailureBacksOff(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghGraphQLRateLimit, code: 1},
		{stderr: "gh: HTTP 500", code: 1},
		{stdout: "dev\n"},
	}}
	host, waits := rateLimitHost(t, script, now)
	if _, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"}); err != nil {
		t.Fatal(err)
	}
	if total := totalWait(*waits); total < time.Minute || total > 72*time.Second {
		t.Fatalf("waited %v, want the unknown-reset backoff", total)
	}
}

func TestSecondaryLimitOnAWriteIsWaitedOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghSecondaryLimit, code: 1},
		{stderr: ghSecondaryLimit, code: 1},
		{stdout: "ok"},
	}}
	host, waits := rateLimitHost(t, script, now)
	if _, err := host.UpdatePR(context.Background(), &scm.PR{Number: "42"}, scm.PRContent{Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if len(script.args) != 3 || len(*waits) != 2 {
		t.Fatalf("calls=%d waits=%v, want two secondary-limit waits and no probe", len(script.args), *waits)
	}
}

func TestRateLimitWaitIsReportedToTheStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(10 * time.Minute)
	script := &rateLimitScript{steps: []ghScriptStep{
		{stderr: ghGraphQLRateLimit, code: 1},
		{stdout: fmt.Sprintf(`{"resources":{"graphql":{"remaining":0,"reset":%d}}}`, reset.Unix())},
		{stdout: "dev\n"},
	}}
	host, _ := rateLimitHost(t, script, now)
	var lines []string
	ctx := netretry.WithWaitNotifier(context.Background(), func(line string) { lines = append(lines, line) })
	if _, err := host.GetPRBaseBranch(ctx, &scm.PR{Number: "42"}); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "waiting for GitHub rate limit until ") ||
		!strings.Contains(lines[0], "class=primary_rate_limit; gh pr view base branch") {
		t.Fatalf("lines=%q, want the wait announced with its operation and reset", lines)
	}
}

func TestRateLimitThatOutlastsTheBudgetFails(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	script := &rateLimitScript{steps: []ghScriptStep{{stderr: ghSecondaryLimit, code: 1}}}
	host, waits := rateLimitHost(t, script, now)
	_, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"})
	if !errors.Is(err, netretry.ErrRateLimitBudgetSpent) {
		t.Fatalf("err=%v, want the bounded rate-limit refusal", err)
	}
	if total := totalWait(*waits); total > netretry.DefaultRateLimitBudget {
		t.Fatalf("waited %v past the budget", total)
	}
}

func TestWriteAndNestedReadsShareRateLimitBudget(t *testing.T) {
	for _, operation := range []string{"create", "rerun"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			reset := func(at time.Time) ghScriptStep {
				return ghScriptStep{stdout: fmt.Sprintf(`{"resources":{"core":{"remaining":0,"reset":%d},"graphql":{"remaining":0,"reset":%d}}}`, at.Unix(), at.Unix())}
			}
			script := &rateLimitScript{steps: []ghScriptStep{
				{stderr: ghRESTRateLimit, code: 1},
				reset(now.Add(20 * time.Minute)),
				{stderr: ghGraphQLRateLimit, code: 1},
				reset(now.Add(70 * time.Minute)),
			}}
			host, waits := rateLimitHost(t, script, now)
			host.repo = "test/repo"
			var err error
			if operation == "create" {
				_, err = host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "test"})
			} else {
				err = host.RerunCheck(context.Background(), nil, scm.Check{RunAttempt: 1, Link: "https://github.com/test/repo/actions/runs/9"})
			}
			if !errors.Is(err, netretry.ErrRateLimitBudgetSpent) {
				t.Fatalf("err=%v, want nested lookup to refuse a reset beyond the shared budget", err)
			}
			if got := totalWait(*waits); got < 20*time.Minute || got > 21*time.Minute || len(script.args) != 4 {
				t.Fatalf("waited=%v calls=%d, want only the first reset wait and no outer replay", got, len(script.args))
			}
		})
	}
}
