// Package netretry bounds retries of transient network failures.
//
// The pipeline talks to GitHub through Git (fetch, ls-remote, push) and the gh
// CLI (GraphQL and REST reads, PR view/create). A single TLS handshake timeout,
// i/o timeout or 5xx otherwise fails the whole run and throws away completed
// review, test and lint work, so every one of those calls goes through Do.
//
// Only a transient network failure is retried. Authentication and permission
// failures, a rejected or non-fast-forward push, a merge conflict and a
// validation error are returned to the caller on the first attempt: retrying
// them changes nothing and can mask the real cause. A caller that replays a
// write must re-check the remote state first; this package only decides
// whether the failure is transient.
package netretry

import (
	"context"
	"log"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
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
	// ClassRateLimit covers a secondary rate limit that names Retry-After.
	ClassRateLimit Class = "secondary_rate_limit"
)

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
}

// Do calls call until it succeeds, the failure is not transient, or the
// attempt budget is spent. A cancelled context stops the call at once,
// including during a backoff wait, and is returned as its own cause.
func Do[T any](ctx context.Context, opts Options, call func() (T, error)) (T, error) {
	var zero T
	attempts := opts.attempts()
	var last T
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		value, callErr := call()
		last, err = value, callErr
		if err == nil {
			return value, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
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
	}
	if opts.Exhausted != nil {
		return last, opts.Exhausted(attempts, err)
	}
	return last, err
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
	if err == nil {
		return ""
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
	"http 401",
	"http 403",
	"http 404",
	"http 422",
	"non-fast-forward",
	"[rejected]",
	"failed to push some refs",
	"pre-receive hook declined",
	"protected branch",
	"merge conflict",
	"conflict",
	"validation failed",
}

var http5xx = regexp.MustCompile(`\b(?:http|http error|status|status code|response code|returned error)[:= ]+5\d\d\b`)

// curlTransportFailure matches the transient libcurl exit codes Git reports for
// a dropped transfer: 18 (partial file), 52 (empty reply), 55 (send failure),
// 56 (receive failure) and 92 (HTTP/2 stream error). A 413 or another terminal
// HTTP status never matches.
var curlTransportFailure = regexp.MustCompile(`rpc failed; curl (?:18|52|55|56|92)\b`)

// ClassifyText classifies one failure message. A transient class is returned
// only for the failures a retry can fix.
func ClassifyText(text string) Class {
	lower := strings.ToLower(text)
	if lower == "" {
		return ""
	}
	if secondaryRateLimit(lower) {
		return ClassRateLimit
	}
	for _, veto := range hardVetoes {
		if strings.Contains(lower, veto) {
			return ""
		}
	}
	switch {
	case strings.Contains(lower, "tls handshake timeout"),
		strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "operation timed out"),
		strings.Contains(lower, "connection timed out"),
		strings.Contains(lower, "timeout was reached"),
		strings.Contains(lower, "client.timeout exceeded"),
		strings.Contains(lower, "deadline exceeded while waiting for response headers"):
		return ClassTimeout
	case strings.Contains(lower, "connection reset by peer"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "couldn't connect to server"),
		strings.Contains(lower, "gnutls_handshake"),
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

// secondaryRateLimit recognises GitHub's secondary rate limit only when the
// response named how long to wait; an unqualified 403 stays a permission
// failure.
func secondaryRateLimit(lower string) bool {
	retryAfter := strings.Contains(lower, "retry-after") || strings.Contains(lower, "retry after")
	if !retryAfter {
		return false
	}
	return strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "http 429") || strings.Contains(lower, "rate limit exceeded")
}
