package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
