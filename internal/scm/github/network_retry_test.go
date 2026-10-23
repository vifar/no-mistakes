package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// ghReadTimeout is the failure shape a GraphQL read produced on 2026-10-23
// (run 01M5M4T3YAVBP6Q17C8HFKN5XF): a dial timeout, not an authentication
// refusal.
const ghReadTimeout = `Post "https://api.github.com/graphql": dial tcp 140.82.112.5:443: i/o timeout`

type ghScriptStep struct {
	stdout string
	stderr string
	code   int
}

// sequencedGHFactory answers each gh call with the next scripted step, then
// repeats the last step. It returns the call count.
func sequencedGHFactory(t *testing.T, steps []ghScriptStep) (CmdFactory, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	factory := func(ctx context.Context, name string, _ ...string) *exec.Cmd {
		mu.Lock()
		index := calls
		calls++
		mu.Unlock()
		step := steps[len(steps)-1]
		if index < len(steps) {
			step = steps[index]
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", name)
		cmd.Env = append(os.Environ(),
			"GITHUB_TEST_HELPER=1",
			"GITHUB_TEST_STDOUT="+step.stdout,
			"GITHUB_TEST_STDERR="+step.stderr,
			fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", step.code),
		)
		return cmd
	}
	return factory, count
}

func fastRetryHost(factory CmdFactory) *Host {
	host := New(factory, nil, "", "")
	host.retryBase = time.Millisecond
	host.retryMax = time.Millisecond
	return host
}

func TestTransientNetworkFailureIsRetriedThenSucceeds(t *testing.T) {
	factory, calls := sequencedGHFactory(t, []ghScriptStep{
		{stderr: ghReadTimeout, code: 1},
		{stderr: "net/http: TLS handshake timeout", code: 1},
		{stdout: "dev\n"},
	})
	host := fastRetryHost(factory)
	base, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"})
	if err != nil || base != "dev" {
		t.Fatalf("base=%q err=%v, want the read to recover", base, err)
	}
	if got := calls(); got != 3 {
		t.Fatalf("gh calls = %d, want 3 (two transient failures then success)", got)
	}
}

func TestAuthenticationFailureIsNotRetried(t *testing.T) {
	for name, detail := range map[string]string{
		"bad credentials": "HTTP 401: Bad credentials",
		"forbidden":       "HTTP 403: Resource not accessible by integration",
		"repo missing":    "remote: Repository not found.",
		"validation":      "HTTP 422: Validation Failed",
	} {
		t.Run(name, func(t *testing.T) {
			factory, calls := sequencedGHFactory(t, []ghScriptStep{{stderr: detail, code: 1}})
			host := fastRetryHost(factory)
			if _, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatal("want the read to fail")
			}
			if got := calls(); got != 1 {
				t.Fatalf("gh calls = %d, want 1 (%s is never retried)", got, name)
			}
		})
	}
}

func TestCreatePRRetryReusesThePullRequestTheLostResponseOpened(t *testing.T) {
	// The first create times out after GitHub accepted it; the retry must find
	// that PR instead of opening a duplicate.
	factory, calls := sequencedGHFactory(t, []ghScriptStep{
		{stderr: `Post "https://api.github.com/graphql": dial tcp 140.82.112.5:443: i/o timeout`, code: 1},
		{stdout: `[{"number":7,"url":"https://github.com/test/repo/pull/7","baseRefName":"main"}]`},
	})
	host := fastRetryHost(factory)
	host.repo = "test/repo"
	pr, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pr.URL != "https://github.com/test/repo/pull/7" || pr.Number != "7" {
		t.Fatalf("pr=%+v, want the PR the lost response opened", pr)
	}
	if got := calls(); got != 2 {
		t.Fatalf("gh calls = %d, want create then the reuse lookup", got)
	}
}

func TestCancelledRetryStopsAtOnce(t *testing.T) {
	factory, calls := sequencedGHFactory(t, []ghScriptStep{{stderr: ghReadTimeout, code: 1}})
	host := New(factory, nil, "", "")
	host.retryBase = time.Hour
	host.retryMax = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := host.GetPRBaseBranch(ctx, &scm.PR{Number: "42"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want cancellation", err)
	}
	if got := calls(); got != 1 {
		t.Fatalf("gh calls = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry kept waiting %v after cancellation", elapsed)
	}
}

func TestCancelledGitHubCommandsNeverReportSuccess(t *testing.T) {
	for name, invoke := range map[string]func(context.Context, *Host) error{
		"update": func(ctx context.Context, h *Host) error {
			_, err := h.UpdatePR(ctx, &scm.PR{Number: "7"}, scm.PRContent{Body: "b"})
			return err
		},
		"retarget": func(ctx context.Context, h *Host) error { return h.SetPRBaseBranch(ctx, &scm.PR{Number: "7"}, "main") },
		"content": func(ctx context.Context, h *Host) error {
			_, err := h.GetPRContent(ctx, &scm.PR{Number: "7"})
			return err
		},
		"base": func(ctx context.Context, h *Host) error {
			_, err := h.GetPRBaseBranch(ctx, &scm.PR{Number: "7"})
			return err
		},
		"auth": func(ctx context.Context, h *Host) error { return h.Available(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			factory, calls := sequencedGHFactory(t, []ghScriptStep{{}})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := invoke(ctx, fastRetryHost(factory))
			if !errors.Is(err, context.Canceled) || calls() != 0 {
				t.Fatalf("err=%v calls=%d, want cancellation without a command", err, calls())
			}
		})
		t.Run(name+" preserves failure", func(t *testing.T) {
			factory, calls := sequencedGHFactory(t, []ghScriptStep{{stderr: "HTTP 403: Resource not accessible by integration", code: 1}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			host := fastRetryHost(func(_ context.Context, name string, args ...string) *exec.Cmd {
				cmd := factory(context.Background(), name, args...)
				cancel()
				return cmd
			})
			err := invoke(ctx, host)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(err.Error(), "HTTP 403") || calls() != 1 {
				t.Fatalf("err=%v calls=%d, want the independent command failure preserved", err, calls())
			}
		})
	}
}

func TestCreatePRRetryFindsRetargetedPRAndFailsClosedOnLookupErrors(t *testing.T) {
	for name, response := range map[string]ghScriptStep{
		"retargeted": {stdout: `[{"number":7,"url":"https://github.com/test/repo/pull/7","baseRefName":"dev"}]`},
		"unreadable": {stderr: "HTTP 403: Resource not accessible by integration", code: 1},
		"malformed":  {stdout: `invalid JSON`},
		"absent":     {stdout: `[]`},
	} {
		t.Run(name, func(t *testing.T) {
			factory, calls := sequencedGHFactory(t, []ghScriptStep{{stderr: ghReadTimeout, code: 1}, response, {stdout: "https://github.com/test/repo/pull/8"}})
			host := fastRetryHost(func(ctx context.Context, cmd string, args ...string) *exec.Cmd {
				if len(args) > 1 && args[0] == "pr" && args[1] == "list" {
					for _, arg := range args {
						if arg == "--base" {
							t.Fatal("recovery lookup must find a PR after retargeting")
						}
					}
				}
				return factory(ctx, cmd, args...)
			})
			host.repo = "test/repo"
			pr, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "t", Body: "b"})
			if name == "unreadable" || name == "malformed" {
				if err == nil || calls() != 2 {
					t.Fatalf("pr=%+v err=%v calls=%d, want lookup failure without another create", pr, err, calls())
				}
			} else if name == "retargeted" {
				if err != nil || pr == nil || pr.BaseBranch != "dev" || calls() != 2 {
					t.Fatalf("pr=%+v err=%v calls=%d, want retargeted PR reused", pr, err, calls())
				}
			} else if err != nil || pr == nil || pr.Number != "8" || calls() != 3 {
				t.Fatalf("pr=%+v err=%v calls=%d, want create after confirmed absence", pr, err, calls())
			}
		})
	}
}
