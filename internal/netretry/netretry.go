// Package netretry bounds retries of transient network failures.
//
// The pipeline talks to GitHub through Git (fetch, ls-remote, push) and the gh
// CLI (GraphQL and REST reads, PR view/create). A single TLS handshake timeout,
// i/o timeout or 5xx otherwise fails the whole run and throws away completed
// review, test and lint work, so every one of those calls goes through Do.
//
// Only a transient network failure is retried. A GitHub rate limit is
// transient too, but it is not spent from the attempt budget: Do waits it out
// (until the primary limit's reset, or a secondary limit's Retry-After or a
// backoff from DefaultRateLimitBackoff), bounded in total by
// DefaultRateLimitBudget, and reports the wait through the context's
// WaitNotifier so a parked run says what it is waiting for. Authentication and permission
// failures, a rejected or non-fast-forward push, a merge conflict and a
// validation error are returned to the caller on the first attempt: retrying
// them changes nothing and can mask the real cause. A caller that replays a
// write must re-check the remote state first; this package only decides
// whether the failure is transient.
package netretry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// DefaultAttempts is the total number of attempts, including the first.
	DefaultAttempts = 3
	// DefaultBase is the wait before the second attempt; it doubles from there.
	DefaultBase = 2 * time.Second
	// DefaultMax caps one backoff delay.
	DefaultMax = 8 * time.Second
	// DefaultJitter spreads retries by up to 20% so parallel runs do not
	// re-stampede a recovering endpoint together.
	DefaultJitter = 0.2
)

// Class names the network failure a retry decision was made from.
type Class string

const (
	// ClassTimeout covers a TLS handshake timeout, an i/o timeout, an
	// operation timeout and a client-side request timeout.
	ClassTimeout Class = "timeout"
	// ClassConnection covers a reset or refused connection.
	ClassConnection Class = "connection"
	// ClassDNS covers a name-resolution failure.
	ClassDNS Class = "dns"
	// ClassHTTP5xx covers a 5xx response from the GitHub API.
	ClassHTTP5xx Class = "http_5xx"
	// ClassRateLimit covers GitHub's secondary rate limit: too many requests
	// or points per minute, or too much concurrency. Its own Retry-After is
	// honored; without one the wait backs off from DefaultRateLimitBackoff.
	ClassRateLimit Class = "secondary_rate_limit"
	// ClassPrimaryRateLimit covers the hourly primary rate limit ("API rate
	// limit exceeded"). The wait lasts until the limit resets.
	ClassPrimaryRateLimit Class = "primary_rate_limit"
)

const (
	// DefaultRateLimitBudget caps the total time one call waits for rate
	// limits. It covers a full hourly primary window plus slack.
	DefaultRateLimitBudget = 65 * time.Minute
	// DefaultRateLimitBackoff is the first wait for a rate limit whose clear
	// time is unknown; it doubles from there up to DefaultRateLimitMaxBackoff.
	DefaultRateLimitBackoff = 60 * time.Second
	// DefaultRateLimitMaxBackoff caps one unknown-clear-time wait.
	DefaultRateLimitMaxBackoff = 15 * time.Minute
	// rateLimitResetSpread spreads the retries of every run that waited for
	// the same reset, so they do not exhaust the fresh window together.
	rateLimitResetSpread = 30 * time.Second
	// rateLimitHeartbeat re-announces a long wait so the step's last activity
	// keeps saying why it is quiet.
	rateLimitHeartbeat = 5 * time.Minute
)

// RateLimited is implemented by an error that carries GitHub's own word on
// when a rate limit clears. A zero value means unknown.
type RateLimited interface {
	RateLimitResetAt() time.Time
	RateLimitRetryAfter() time.Duration
}

type waitNotifierKey struct{}
type rateLimitBudgetKey struct{}

func WithRateLimitBudget(ctx context.Context) context.Context {
	if ctx.Value(rateLimitBudgetKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitBudgetKey{}, new(atomic.Int64))
}

// WithWaitNotifier returns a context whose rate-limit waits are reported to
// notify, one line per wait and heartbeat. The pipeline installs the step's
// log here, so the wait is the step's visible last activity.
func WithWaitNotifier(ctx context.Context, notify func(string)) context.Context {
	if notify == nil {
		return ctx
	}
	return context.WithValue(ctx, waitNotifierKey{}, notify)
}

func waitNotifier(ctx context.Context) func(string) {
	notify, _ := ctx.Value(waitNotifierKey{}).(func(string))
	return notify
}

// Options configures one retried call. The zero value retries transient
// network failures up to DefaultAttempts with exponential backoff.
type Options struct {
	// Operation names the call in retry log lines, for example "git fetch".
	Operation string
	// Attempts is the total number of attempts; zero means DefaultAttempts.
	Attempts int
	// Base is the first backoff; zero means DefaultBase.
	Base time.Duration
	// Max caps one backoff; zero means DefaultMax.
	Max time.Duration
	// Jitter is the fractional spread added to a backoff; zero means
	// DefaultJitter. Negative disables jitter.
	Jitter float64
	// Log receives one line per retry. Nil writes through the standard
	// logger, which the daemon captures.
	Log func(string)
	// Retryable overrides the transient network classification. It is used by
	// callers that already typed their failure, such as the PR-context
	// credential-store timeout.
	Retryable func(error) bool
	// Describe overrides the error class name in retry log lines.
	Describe func(error) string
	// Exhausted wraps the last error once every attempt has failed. It must
	// not change the cause: wrap with %w.
	Exhausted func(attempts int, err error) error
	// RateLimitBudget caps the total rate-limit wait; zero means
	// DefaultRateLimitBudget and negative returns a rate limit at once.
	RateLimitBudget time.Duration
	// ProbeRateLimit may enrich a rate-limit failure with its clear time (for
	// example from `gh api rate_limit`) or reclassify it. It returns the error
	// to wait on; it must keep the cause with %w.
	ProbeRateLimit func(ctx context.Context, err error) error
	// Wait and Now are test hooks for the rate-limit wait; nil uses a timer
	// and time.Now.
	Wait func(ctx context.Context, d time.Duration) error
	Now  func() time.Time
}

// ErrRateLimitBudgetSpent marks a rate limit that outlasted the wait budget.
var ErrRateLimitBudgetSpent = errors.New("GitHub rate limit did not clear within the wait budget")

// Do calls call until it succeeds, the failure is not transient, or the
// attempt budget is spent. A rate limit is waited out without spending an
// attempt, within the rate-limit budget. A cancelled context stops the call at
// once, including during a backoff or rate-limit wait, and is returned as its
// own cause.
func Do[T any](ctx context.Context, opts Options, call func() (T, error)) (T, error) {
	var zero T
	attempts := opts.attempts()
	var last T
	var err error
	ctx = WithRateLimitBudget(ctx)
	rateLimitWaited := ctx.Value(rateLimitBudgetKey{}).(*atomic.Int64)
	rateLimitWaits := 0
	for attempt := 1; attempt <= attempts; {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		value, callErr := call()
		last, err = value, callErr
		if err == nil {
			return value, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return last, errors.Join(err, ctxErr)
		}
		if errors.Is(err, ErrRateLimitBudgetSpent) {
			return last, err
		}
		if opts.Retryable == nil && RateLimitClass(err) != "" {
			if opts.ProbeRateLimit != nil {
				err = opts.ProbeRateLimit(ctx, err)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return last, errors.Join(err, ctxErr)
				}
			}
			if class := RateLimitClass(err); class != "" {
				delay, until := opts.rateLimitDelay(err, rateLimitWaits)
				budget := opts.rateLimitBudget()
				waited := time.Duration(rateLimitWaited.Load())
				if budget < 0 || delay > budget-waited {
					return last, fmt.Errorf("%w (%s waited): %w", ErrRateLimitBudgetSpent, waited.Round(time.Second), err)
				}
				if deadline, ok := ctx.Deadline(); ok && opts.now().Add(delay).After(deadline) {
					// The caller's own bound ends before the limit clears:
					// waiting would only turn the rate limit into a timeout.
					return last, err
				}
				if waitErr := opts.waitForRateLimit(ctx, class, delay, until, err); waitErr != nil {
					return zero, waitErr
				}
				rateLimitWaited.Add(int64(delay))
				rateLimitWaits++
				continue
			}
		}
		if !opts.retryable(err) {
			// A failure a retry cannot fix is the caller's answer on the first
			// attempt, unwrapped: it is not an exhausted budget.
			return last, err
		}
		if attempt == attempts {
			break
		}
		delay := opts.Delay(attempt)
		opts.logf(attempt, attempts, delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
		attempt++
	}
	if opts.Exhausted != nil {
		return last, opts.Exhausted(attempts, err)
	}
	return last, err
}

// RateLimitClass returns ClassPrimaryRateLimit or ClassRateLimit when err is a
// GitHub rate limit, and "" otherwise.
func RateLimitClass(err error) Class {
	switch class := Class(Describe(err)); class {
	case ClassRateLimit, ClassPrimaryRateLimit:
		return class
	}
	return ""
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) rateLimitBudget() time.Duration {
	if o.RateLimitBudget == 0 {
		return DefaultRateLimitBudget
	}
	return o.RateLimitBudget
}

// rateLimitDelay returns how long to wait for a rate limit and the moment it
// is expected to clear. A known reset wins; otherwise the wait starts at the
// Retry-After (or DefaultRateLimitBackoff) and doubles while the limit holds,
// never shorter than a later Retry-After.
func (o Options) rateLimitDelay(err error, waits int) (time.Duration, time.Time) {
	now := o.now()
	var limited RateLimited
	if errors.As(err, &limited) {
		if reset := limited.RateLimitResetAt(); !reset.IsZero() {
			delay := reset.Sub(now)
			if delay < time.Second {
				delay = time.Second
			}
			delay += time.Duration(rand.Int64N(int64(rateLimitResetSpread)))
			return delay, now.Add(delay)
		}
	}
	// GitHub's guidance for a secondary limit: wait at least Retry-After (or a
	// minute without one), and wait exponentially longer while it persists.
	delay := DefaultRateLimitBackoff
	if limited != nil {
		if after := limited.RateLimitRetryAfter(); after > 0 {
			delay = after
		}
	}
	for i := 0; i < waits && delay < DefaultRateLimitMaxBackoff; i++ {
		delay *= 2
	}
	if delay > DefaultRateLimitMaxBackoff {
		delay = DefaultRateLimitMaxBackoff
	}
	if limited != nil {
		delay = max(delay, limited.RateLimitRetryAfter())
	}
	delay += time.Duration(float64(delay) * DefaultJitter * rand.Float64())
	return delay, now.Add(delay)
}

// waitForRateLimit sleeps for delay, announcing the wait when it starts and
// again every rateLimitHeartbeat so a parked step keeps saying why it is quiet.
func (o Options) waitForRateLimit(ctx context.Context, class Class, delay time.Duration, until time.Time, err error) error {
	operation := o.Operation
	if operation == "" {
		operation = "network call"
	}
	announce := func(prefix string) {
		line := prefix + " for GitHub rate limit until " + until.Format("2006-01-02 15:04:05 MST") +
			" (class=" + string(class) + "; " + operation + "): " + err.Error()
		if notify := waitNotifier(ctx); notify != nil {
			notify(line)
			return
		}
		if o.Log != nil {
			o.Log(line)
			return
		}
		log.Printf("no-mistakes network retry: %s", line)
	}
	wait := o.Wait
	if wait == nil {
		wait = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	announce("waiting")
	for remaining := delay; remaining > 0; {
		chunk := min(remaining, rateLimitHeartbeat)
		if waitErr := wait(ctx, chunk); waitErr != nil {
			return waitErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		remaining -= chunk
		if remaining > 0 {
			announce("still waiting")
		}
	}
	return nil
}

// Delay returns the wait before the retry that follows the given 1-based
// attempt.
func (o Options) Delay(attempt int) time.Duration {
	base := o.Base
	if base <= 0 {
		base = DefaultBase
	}
	max := o.Max
	if max <= 0 {
		max = DefaultMax
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			delay = max
			break
		}
	}
	if delay > max {
		delay = max
	}
	jitter := o.Jitter
	if jitter == 0 {
		jitter = DefaultJitter
	}
	if jitter > 0 {
		delay += time.Duration(float64(delay) * jitter * rand.Float64())
		if delay > max {
			delay = max
		}
	}
	return delay
}

func (o Options) attempts() int {
	if o.Attempts <= 0 {
		return DefaultAttempts
	}
	return o.Attempts
}

func (o Options) retryable(err error) bool {
	if err == nil {
		return false
	}
	if o.Retryable != nil {
		return o.Retryable(err)
	}
	return Retryable(err)
}

func (o Options) logf(attempt, attempts int, delay time.Duration, err error) {
	class := Describe(err)
	if o.Describe != nil {
		class = o.Describe(err)
	}
	operation := o.Operation
	if operation == "" {
		operation = "network call"
	}
	line := "retrying " + operation + ": attempt " + strconv.Itoa(attempt+1) + " of " + strconv.Itoa(attempts) +
		" in " + delay.Round(time.Millisecond).String() + " (class=" + class + "): " + err.Error()
	if o.Log != nil {
		o.Log(line)
		return
	}
	log.Printf("no-mistakes network retry: %s", line)
}

// Retryable reports whether err is a transient network failure.
func Retryable(err error) bool {
	return Describe(err) != ""
}

// Describe returns the transient failure class of err, or "" when err is not a
// transient network failure. The class is what a retry log line reports.
func Describe(err error) string {
	if err == nil || errors.Is(err, ErrRateLimitBudgetSpent) {
		return ""
	}
	var classified interface{ RetryClass() Class }
	if errors.As(err, &classified) {
		return string(classified.RetryClass())
	}
	var diagnostic interface{ RetryDiagnostic() string }
	if errors.As(err, &diagnostic) {
		return string(ClassifyText(diagnostic.RetryDiagnostic()))
	}
	return string(ClassifyText(err.Error()))
}

// hardVetoes name failures a retry cannot fix: a credential or permission
// problem, a rejected publication, or a conflict. They are checked before the
// transient patterns because several of these messages also carry the word
// "timeout" (a keyring timeout, for example).
var hardVetoes = []string{
	"(keyring)",
	"bad credentials",
	"authentication failed",
	"could not read username",
	"could not read password",
	"not logged in",
	"failed to log in",
	"permission denied",
	"permission to ",
	"repository not found",
	"non-fast-forward",
	"[rejected]",
	// A ruleset or hook rejection is a decision about the push, not a
	// transport failure, even when its message names a required check that
	// happens to contain a timeout word.
	"[remote rejected]",
	"push declined",
	"pre-receive hook declined",
	"protected branch",
	"merge conflict",
	"validation failed",
}

var diagnosticURLs = regexp.MustCompile(`'(?:[a-z][a-z0-9+.-]*://)[^']*'|"(?:[a-z][a-z0-9+.-]*://)[^"]*"|[a-z][a-z0-9+.-]*://[^\s'"<>]+|[a-z0-9._-]+@[a-z0-9.-]+:[^\s'"<>]+`)
var http4xx = regexp.MustCompile(`\b(?:http(?:/[0-9.]+)?|http error|status|status code|response code|returned error)[:= ]+(4\d\d)\b`)

var http5xx = regexp.MustCompile(`\b(?:http(?:/[0-9.]+)?|http error|status|status code|response code|returned error)[:= ]+5\d\d\b`)

// curlTransportFailure matches the transient libcurl exit codes Git reports for
// a dropped transfer: 18 (partial file), 52 (empty reply), 55 (send failure),
// 56 (receive failure) and 92 (HTTP/2 stream error). A 413 or another terminal
// HTTP status never matches.
var curlTransportFailure = regexp.MustCompile(`rpc failed; curl (?:18|52|55|56|92)\b`)
var curlTimeout = regexp.MustCompile(`\bcurl 28\b`)

// ClassifyText classifies one failure message. A transient class is returned
// only for the failures a retry can fix.
func ClassifyText(text string) Class {
	lower := diagnosticURLs.ReplaceAllString(strings.ToLower(text), "")
	if lower == "" {
		return ""
	}
	for _, veto := range hardVetoes {
		if strings.Contains(lower, veto) {
			return ""
		}
	}
	rateLimit := rateLimitText(lower)
	for _, status := range http4xx.FindAllStringSubmatch(lower, -1) {
		if rateLimit == "" || status[1] != "403" && status[1] != "429" {
			return ""
		}
	}
	if rateLimit != "" {
		return rateLimit
	}
	switch {
	case curlTimeout.MatchString(lower),
		strings.Contains(lower, "tls handshake timeout"),
		strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "operation timed out"),
		strings.Contains(lower, "operation too slow. less than"),
		strings.Contains(lower, "connection timed out"),
		strings.Contains(lower, "timeout was reached"),
		strings.Contains(lower, "client.timeout exceeded"),
		strings.Contains(lower, "deadline exceeded while waiting for response headers"):
		return ClassTimeout
	case strings.Contains(lower, "connection reset by peer"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "couldn't connect to server"),
		strings.Contains(lower, "the remote end hung up unexpectedly"),
		strings.Contains(lower, "unexpected disconnect while reading sideband packet"),
		curlTransportFailure.MatchString(lower),
		strings.Contains(lower, "no route to host"):
		return ClassConnection
	case strings.Contains(lower, "could not resolve host"),
		strings.Contains(lower, "temporary failure in name resolution"),
		strings.Contains(lower, "no such host"):
		return ClassDNS
	case strings.Contains(lower, "502 bad gateway"),
		strings.Contains(lower, "503 service unavailable"),
		strings.Contains(lower, "504 gateway timeout"),
		strings.Contains(lower, "500 internal server error"),
		http5xx.MatchString(lower):
		return ClassHTTP5xx
	case strings.Contains(lower, "unable to access") &&
		(strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")):
		return ClassTimeout
	}
	return ""
}

var primaryRateLimit = regexp.MustCompile(`\bapi rate limit (?:already )?exceeded\b`)
var tooManyRequests = regexp.MustCompile(`\b(?:http(?:/[0-9.]+)?|status|status code)[:= ]+429\b|\b429 too many requests\b`)

// rateLimitText classifies GitHub's own rate-limit messages. "secondary rate
// limit" and abuse detection are the secondary limit, as is a bare 429. "API
// rate limit (already) exceeded" names the primary limit, but GitHub also
// returns it for a secondary limit while the hourly budget still has room;
// the GitHub host's probe of `gh api rate_limit` reclassifies that case.
func rateLimitText(lower string) Class {
	switch {
	case strings.Contains(lower, "secondary rate limit"),
		strings.Contains(lower, "abuse detection mechanism"),
		tooManyRequests.MatchString(lower):
		return ClassRateLimit
	case primaryRateLimit.MatchString(lower):
		return ClassPrimaryRateLimit
	}
	return ""
}
