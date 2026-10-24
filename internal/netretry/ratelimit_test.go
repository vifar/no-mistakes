package netretry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The two messages GitHub returned on 2026-10-24 when ~14 runs shared one
// account: the primary hourly GraphQL limit really at zero, and the same text
// while rateLimit.remaining still read 3954 (the secondary limit).
const (
	ghPrimaryLimit   = "GraphQL: API rate limit already exceeded for user ID 12345.: exit status 1"
	ghSecondaryLimit = "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)"
)

type rateLimitClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *rateLimitClock) options(log *[]string) Options {
	return Options{
		Operation: "gh pr view",
		Base:      time.Millisecond,
		Max:       time.Millisecond,
		Now:       func() time.Time { return c.now },
		Wait: func(_ context.Context, d time.Duration) error {
			c.waits = append(c.waits, d)
			c.now = c.now.Add(d)
			return nil
		},
		Log: func(line string) { *log = append(*log, line) },
	}
}

func (c *rateLimitClock) total() time.Duration {
	var total time.Duration
	for _, d := range c.waits {
		total += d
	}
	return total
}

type resetError struct {
	error
	reset time.Time
	after time.Duration
}

func (e resetError) Unwrap() error                      { return e.error }
func (e resetError) RateLimitResetAt() time.Time        { return e.reset }
func (e resetError) RateLimitRetryAfter() time.Duration { return e.after }

func TestPrimaryRateLimitWaitsUntilResetWithoutSpendingAttempts(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	reset := clock.now.Add(20 * time.Minute)
	var logs []string
	opts := clock.options(&logs)
	probes := 0
	opts.ProbeRateLimit = func(_ context.Context, err error) error {
		probes++
		return resetError{error: err, reset: reset}
	}
	calls := 0
	value, err := Do(context.Background(), opts, func() (string, error) {
		calls++
		if !clock.now.After(reset) {
			return "", errors.New(ghPrimaryLimit)
		}
		return "main", nil
	})
	if err != nil || value != "main" {
		t.Fatalf("value=%q err=%v, want the read to succeed after the reset", value, err)
	}
	if calls != 2 || probes != 1 {
		t.Fatalf("calls=%d probes=%d, want one wait then success", calls, probes)
	}
	if clock.total() < 20*time.Minute || clock.total() > 20*time.Minute+rateLimitResetSpread {
		t.Fatalf("waited %v, want the reset plus at most %v of spread", clock.total(), rateLimitResetSpread)
	}
	for _, d := range clock.waits {
		if d > rateLimitHeartbeat {
			t.Fatalf("one wait chunk was %v, want heartbeats at most every %v", d, rateLimitHeartbeat)
		}
	}
	if len(logs) < 2 || !strings.HasPrefix(logs[0], "waiting for GitHub rate limit until ") ||
		!strings.Contains(logs[0], "class=primary_rate_limit") || !strings.HasPrefix(logs[1], "still waiting for GitHub rate limit until ") {
		t.Fatalf("logs=%q, want the wait and its heartbeats announced", logs)
	}
}

func TestRateLimitWaitDoesNotConsumeTheAttemptBudget(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	calls := 0
	value, err := Do(context.Background(), clock.options(&logs), func() (string, error) {
		calls++
		if calls <= 5 {
			return "", errors.New(ghSecondaryLimit)
		}
		return "ok", nil
	})
	if err != nil || value != "ok" || calls != 6 {
		t.Fatalf("calls=%d value=%q err=%v, want five rate-limit waits then success", calls, value, err)
	}
}

func TestSecondaryRateLimitBacksOffFromSixtySeconds(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	calls := 0
	_, err := Do(context.Background(), clock.options(&logs), func() (string, error) {
		calls++
		if calls <= 3 {
			return "", errors.New(ghSecondaryLimit)
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Waits are announced once each (all shorter than a heartbeat).
	if len(clock.waits) != 3 {
		t.Fatalf("waits=%v, want three", clock.waits)
	}
	for i, base := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		if got := clock.waits[i]; got < base || got > base+time.Duration(float64(base)*DefaultJitter) {
			t.Fatalf("wait %d = %v, want %v plus at most %.0f%% jitter", i, got, base, DefaultJitter*100)
		}
	}
	if !strings.Contains(logs[0], "class=secondary_rate_limit") {
		t.Fatalf("logs=%q", logs)
	}
}

func TestSecondaryRateLimitHonorsRetryAfter(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	calls := 0
	_, err := Do(context.Background(), clock.options(&logs), func() (string, error) {
		calls++
		if calls == 1 {
			return "", resetError{error: errors.New(ghSecondaryLimit), after: 7 * time.Second}
		}
		return "ok", nil
	})
	if err != nil || len(clock.waits) != 1 || clock.waits[0] < 7*time.Second || clock.waits[0] > 9*time.Second {
		t.Fatalf("err=%v waits=%v, want one Retry-After wait of about 7s", err, clock.waits)
	}
}

func TestProbeReclassifiesAPrimaryMessageAsSecondary(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	opts := clock.options(&logs)
	opts.ProbeRateLimit = func(_ context.Context, err error) error {
		return reclassified{error: err}
	}
	calls := 0
	_, err := Do(context.Background(), opts, func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New(ghPrimaryLimit)
		}
		return "ok", nil
	})
	if err != nil || len(logs) == 0 || !strings.Contains(logs[0], "class=secondary_rate_limit") {
		t.Fatalf("err=%v logs=%q, want a secondary-limit wait", err, logs)
	}
}

type reclassified struct{ error }

func (e reclassified) Unwrap() error     { return e.error }
func (e reclassified) RetryClass() Class { return ClassRateLimit }

func TestRateLimitWaitIsBoundedByItsBudget(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	calls := 0
	_, err := Do(context.Background(), clock.options(&logs), func() (string, error) {
		calls++
		return "", errors.New(ghSecondaryLimit)
	})
	if !errors.Is(err, ErrRateLimitBudgetSpent) || !strings.Contains(err.Error(), "secondary rate limit") {
		t.Fatalf("err=%v, want the budget refusal carrying the cause", err)
	}
	if clock.total() > DefaultRateLimitBudget {
		t.Fatalf("waited %v, over the %v budget", clock.total(), DefaultRateLimitBudget)
	}
	if calls < 4 {
		t.Fatalf("calls=%d, want several waits before the budget is spent", calls)
	}
}

func TestRateLimitWaitStopsAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opts := Options{Wait: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}
	opts.Log = func(string) {}
	calls := 0
	_, err := Do(ctx, opts, func() (string, error) {
		calls++
		return "", errors.New(ghSecondaryLimit)
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v, want the cancellation at once", calls, err)
	}
}

func TestRateLimitIsReturnedWhenTheCallersDeadlineEndsFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waited := false
	_, err := Do(ctx, Options{Wait: func(context.Context, time.Duration) error { waited = true; return nil }}, func() (string, error) {
		return "", errors.New(ghSecondaryLimit)
	})
	if waited || err == nil || !strings.Contains(err.Error(), "secondary rate limit") {
		t.Fatalf("waited=%v err=%v, want the rate limit returned without waiting past the deadline", waited, err)
	}
}

func TestRateLimitWaitGoesToTheContextNotifier(t *testing.T) {
	var notified, logged []string
	ctx := WithWaitNotifier(context.Background(), func(line string) { notified = append(notified, line) })
	calls := 0
	_, err := Do(ctx, Options{
		Wait: func(context.Context, time.Duration) error { return nil },
		Log:  func(line string) { logged = append(logged, line) },
	}, func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New(ghSecondaryLimit)
		}
		return "ok", nil
	})
	if err != nil || len(notified) != 1 || len(logged) != 0 || !strings.HasPrefix(notified[0], "waiting for GitHub rate limit until ") {
		t.Fatalf("err=%v notified=%q logged=%q", err, notified, logged)
	}
}

func TestCustomRetryableNeverWaitsForRateLimits(t *testing.T) {
	waited := false
	_, err := Do(context.Background(), Options{
		Retryable: func(error) bool { return false },
		Wait:      func(context.Context, time.Duration) error { waited = true; return nil },
	}, func() (string, error) {
		return "", errors.New(ghSecondaryLimit)
	})
	if waited || err == nil {
		t.Fatalf("waited=%v err=%v, want a typed-retry caller to keep its own policy", waited, err)
	}
}

func TestSecondaryRateLimitLongRetryAfterIsAMinimum(t *testing.T) {
	for _, after := range []time.Duration{20 * time.Minute, 70 * time.Minute} {
		t.Run(after.String(), func(t *testing.T) {
			clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
			var logs []string
			calls := 0
			_, err := Do(context.Background(), clock.options(&logs), func() (string, error) {
				calls++
				if calls == 1 {
					return "", resetError{error: errors.New(ghSecondaryLimit), after: after}
				}
				return "ok", nil
			})
			if after > 65*time.Minute {
				if !errors.Is(err, ErrRateLimitBudgetSpent) || calls != 1 || clock.total() != 0 {
					t.Fatalf("err=%v calls=%d waited=%v, want refusal without an early retry", err, calls, clock.total())
				}
			} else if err != nil || calls != 2 || clock.total() < after {
				t.Fatalf("err=%v calls=%d waited=%v, want retry after %v", err, calls, clock.total(), after)
			}
		})
	}
}

func TestNestedRateLimitBudgetExhaustionIsTerminal(t *testing.T) {
	clock := &rateLimitClock{now: time.Unix(1_800_000_000, 0)}
	var logs []string
	opts := clock.options(&logs)
	ctx := WithRateLimitBudget(context.Background())
	outerCalls := 0
	_, err := Do(ctx, opts, func() (string, error) {
		outerCalls++
		return Do(ctx, opts, func() (string, error) {
			return "", errors.New(ghSecondaryLimit)
		})
	})
	if !errors.Is(err, ErrRateLimitBudgetSpent) || outerCalls != 1 || clock.total() > 65*time.Minute {
		t.Fatalf("err=%v outer calls=%d waited=%v", err, outerCalls, clock.total())
	}
	if Retryable(err) || RateLimitClass(err) != "" {
		t.Fatalf("exhausted error remains retryable: %v", err)
	}
}
