package netretry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyTransientNetworkFailures(t *testing.T) {
	cases := map[string]Class{
		`git fetch --no-tags origin +refs/heads/dev:refs/remotes/origin/dev: exit status 128: fatal: unable to access 'https://github.com/vifar/controller.git/': Operation timed out after 30000 milliseconds with 0 bytes received`: ClassTimeout,
		`Post "https://api.github.com/graphql": dial tcp 140.82.112.5:443: i/o timeout`:             ClassTimeout,
		`net/http: TLS handshake timeout`:                                                           ClassTimeout,
		`read tcp 10.0.0.2:52344->140.82.112.5:443: read: connection reset by peer`:                 ClassConnection,
		`dial tcp 140.82.112.5:443: connect: connection refused`:                                    ClassConnection,
		`fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com`: ClassDNS,
		`HTTP 502: Bad Gateway`:                          ClassHTTP5xx,
		`HTTP 503: Service Unavailable`:                  ClassHTTP5xx,
		`HTTP 504: Gateway Timeout`:                      ClassHTTP5xx,
		`unexpected status code 500 from api.github.com`: ClassHTTP5xx,
		`You have exceeded a secondary rate limit. Please wait before you try again. Retry-After: 60`:                                         ClassRateLimit,
		`error: RPC failed; curl 56 OpenSSL SSL_read: Connection reset by peer`:                                                               ClassConnection,
		`error: RPC failed; curl 18 transfer closed with outstanding read data remaining`:                                                     ClassConnection,
		`fatal: unable to access 'https://github.com/x/y.git/': Failed to connect to github.com port 443 after 75001 ms: Timeout was reached`: ClassTimeout,
		`fatal: the remote end hung up unexpectedly`:                                                                                          ClassConnection,
		`HTTP 429: You have exceeded a secondary rate limit; Retry-After: 30`:                                                                 ClassRateLimit,
		`API rate limit exceeded for user ID 12345.`:                                                                                          ClassPrimaryRateLimit,
		`GraphQL: API rate limit already exceeded for user ID 12345.`:                                                                         ClassPrimaryRateLimit,
		`gh: API rate limit exceeded for user ID 12345. (HTTP 403)`:                                                                           ClassPrimaryRateLimit,
		`You have exceeded a secondary rate limit. Please wait a few minutes before you try again.`:                                           ClassRateLimit,
		`HTTP 429: Too Many Requests; Retry-After: 30`:                                                                                        ClassRateLimit,
		`You have triggered an abuse detection mechanism. Please wait a few minutes before you try again.`:                                    ClassRateLimit,
	}
	for message, want := range cases {
		if got := ClassifyText(message); got != want {
			t.Errorf("ClassifyText(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestClassifyNeverRetriesRealFailures(t *testing.T) {
	messages := []string{
		`HTTP 401: Bad credentials`,
		`HTTP 403: Resource not accessible by integration`,
		`fatal: Authentication failed for 'https://github.com/x/y.git/'`,
		`git@github.com: Permission denied (publickey).`,
		`remote: Repository not found.`,
		`! [rejected]        main -> main (non-fast-forward)`,
		`error: failed to push some refs to 'https://github.com/x/y.git'`,
		`remote: error: GH006: Protected branch update failed`,
		`CONFLICT (content): Merge conflict in internal/git/git.go`,
		`Validation Failed: title can't be blank`,
		`HTTP 422: Unprocessable Entity`,
		`Timeout trying to log in to github.com account vifar (keyring)`,
		`network timeout`,
		`error: RPC failed; HTTP 413 curl 22 The requested URL returned error: 413`,
		`remote: error: GH013: Repository rule violations found for refs/heads/main. : [remote rejected] main -> main (push declined due to repository rule violations)`,
		`! [remote rejected] main -> main (pre-receive hook declined)`,
		`HTTP 401: API rate limit exceeded`,
		`HTTP 404: secondary rate limit`,
		`gnutls_handshake() failed: Error in protocol version`,
		`gh: command not found`,
		``,
	}
	for _, message := range messages {
		if class := ClassifyText(message); class != "" {
			t.Errorf("ClassifyText(%q) = %q, want no retry", message, class)
		}
	}
}

func TestDoEnforcesTransportRetryEligibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		class   Class
	}{
		{"curl low speed timeout", "error: RPC failed; curl 28 Operation too slow. Less than 1000 bytes/sec transferred the last 30 seconds", ClassTimeout},
		{"git low speed timeout", "fatal: unable to access 'https://github.com/x/y.git/': Operation too slow. Less than 1 bytes/sec transferred the last 1 seconds", ClassTimeout},
		{"git low speed with permission failure", "HTTP 403: forbidden; Operation too slow. Less than 1 bytes/sec transferred the last 1 seconds", ""},
		{"git low speed with rejected push", "[remote rejected] main -> main; Operation too slow. Less than 1 bytes/sec transferred the last 1 seconds", ""},
		{"git low speed with conflict", "Merge conflict; Operation too slow. Less than 1 bytes/sec transferred the last 1 seconds", ""},
		{"git low speed with validation failure", "Validation Failed; Operation too slow. Less than 1 bytes/sec transferred the last 1 seconds", ""},
		{"curl timeout with permission failure", "HTTP 403: forbidden; RPC failed; curl 28 Operation too slow", ""},
		{"other curl code", "error: RPC failed; curl 280 unknown failure", ""},
		{"secondary limit with header", "HTTP 403: You have exceeded a secondary rate limit.\nRetry-After: 60", ClassRateLimit},
		{"secondary limit without header", "HTTP 403: You have exceeded a secondary rate limit.", ClassRateLimit},
		{"secondary limit without status or header", "You have exceeded a secondary rate limit.", ClassRateLimit},
		{"secondary limit with prose wait", "HTTP 403: secondary rate limit; retry after a few minutes", ClassRateLimit},
		{"unqualified rate limit with header", "HTTP 429: Too Many Requests; Retry-After: 30", ClassRateLimit},
		{"primary limit with header", "HTTP 403: API rate limit exceeded; Retry-After: 60", ClassPrimaryRateLimit},
		{"gnutls protocol mismatch", "gnutls_handshake() failed: Error in protocol version", ""},
		{"gnutls timeout", "gnutls_handshake() failed: Connection timed out", ClassTimeout},
		{"gnutls reset", "gnutls_handshake() failed: Connection reset by peer", ClassConnection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var logs []string
			failure := errors.New(tc.message)
			value, err := Do(context.Background(), Options{
				Base: time.Millisecond,
				Max:  time.Millisecond,
				Log:  func(line string) { logs = append(logs, line) },
				Wait: func(context.Context, time.Duration) error { return nil },
			}, func() (string, error) {
				calls++
				if calls == 1 {
					return "", failure
				}
				return "recovered", nil
			})
			if tc.class == "" {
				if calls != 1 || !errors.Is(err, failure) || len(logs) != 0 {
					t.Fatalf("calls=%d err=%v logs=%v, want original failure without retry", calls, err, logs)
				}
			} else if calls != 2 || err != nil || value != "recovered" || len(logs) != 1 || !strings.Contains(logs[0], "class="+string(tc.class)) {
				t.Fatalf("calls=%d value=%q err=%v logs=%v, want recovery with class %s", calls, value, err, logs, tc.class)
			}
		})
	}
}

func TestDoRetriesTransientFailureThenSucceeds(t *testing.T) {
	calls := 0
	var logs []string
	value, err := Do(context.Background(), Options{
		Operation: "git fetch",
		Base:      time.Millisecond,
		Max:       time.Millisecond,
		Log:       func(line string) { logs = append(logs, line) },
	}, func() (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("fatal: unable to access 'https://github.com/x/y.git/': Operation timed out")
		}
		return "ok", nil
	})
	if err != nil || value != "ok" || calls != 3 {
		t.Fatalf("value=%q calls=%d err=%v, want a third-attempt success", value, calls, err)
	}
	if len(logs) != 2 {
		t.Fatalf("logs=%v, want one line per retry", logs)
	}
	for i, want := range []string{"attempt 2 of 3", "attempt 3 of 3"} {
		if !strings.Contains(logs[i], want) || !strings.Contains(logs[i], "class=timeout") {
			t.Errorf("log %d = %q, want %q and the error class", i, logs[i], want)
		}
	}
}

func TestDoDoesNotRetryAuthFailure(t *testing.T) {
	calls := 0
	auth := errors.New("HTTP 401: Bad credentials")
	_, err := Do(context.Background(), Options{Base: time.Millisecond}, func() (int, error) {
		calls++
		return 0, auth
	})
	if calls != 1 || !errors.Is(err, auth) {
		t.Fatalf("calls=%d err=%v, want one attempt and the original cause", calls, err)
	}
}

func TestDoStopsAtOnceWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	_, err := Do(ctx, Options{
		Base: time.Hour,
		Max:  time.Hour,
		Log:  func(string) { cancel() },
	}, func() (int, error) {
		calls++
		return 0, errors.New("i/o timeout")
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v, want one attempt and cancellation", calls, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("backoff kept waiting %v after cancellation", elapsed)
	}
}

func TestDoDoesNotStartWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := Do(ctx, Options{}, func() (int, error) { calls++; return 0, nil })
	if calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v, want no attempt", calls, err)
	}
}

func TestDoWrapsExhaustionWithoutLosingTheCause(t *testing.T) {
	calls := 0
	_, err := Do(context.Background(), Options{
		Base: time.Millisecond,
		Max:  time.Millisecond,
		Exhausted: func(attempts int, err error) error {
			return fmt.Errorf("unavailable after %d attempts: %w", attempts, err)
		},
	}, func() (int, error) {
		calls++
		return 0, errors.New("net/http: TLS handshake timeout")
	})
	if calls != DefaultAttempts {
		t.Fatalf("calls=%d, want %d", calls, DefaultAttempts)
	}
	if !strings.Contains(err.Error(), "unavailable after 3 attempts") || !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("err=%v, want the bounded wrapper and the original cause", err)
	}
}

func TestDoNeverWrapsANonRetryableFailureAsExhaustion(t *testing.T) {
	auth := errors.New("HTTP 401: Bad credentials")
	calls := 0
	_, err := Do(context.Background(), Options{
		Base: time.Millisecond,
		Exhausted: func(attempts int, err error) error {
			return fmt.Errorf("unavailable after %d attempts: %w", attempts, err)
		},
	}, func() (int, error) {
		calls++
		return 0, auth
	})
	if calls != 1 || !errors.Is(err, auth) || strings.Contains(err.Error(), "unavailable after") {
		t.Fatalf("calls=%d err=%v, want the raw first-attempt failure, not an exhaustion", calls, err)
	}
}

func TestDoRetryableOverrideKeepsTypedClassification(t *testing.T) {
	typed := errors.New("transient keyring timeout")
	calls := 0
	_, err := Do(context.Background(), Options{
		Base:      time.Millisecond,
		Max:       time.Millisecond,
		Retryable: func(err error) bool { return strings.Contains(err.Error(), "keyring") },
		Describe:  func(error) string { return "transient keyring timeout" },
	}, func() (int, error) {
		calls++
		return 0, typed
	})
	if calls != DefaultAttempts || !errors.Is(err, typed) {
		t.Fatalf("calls=%d err=%v, want the typed class retried to the bound", calls, err)
	}
}

func TestDelayGrowsToTheCap(t *testing.T) {
	opts := Options{Base: 2 * time.Second, Max: 8 * time.Second, Jitter: -1}
	for attempt, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 8 * time.Second} {
		if got := opts.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestDelayAddsBoundedJitter(t *testing.T) {
	opts := Options{Base: 2 * time.Second, Max: 8 * time.Second, Jitter: 0.2}
	for i := 0; i < 50; i++ {
		got := opts.Delay(1)
		if got < 2*time.Second || got > time.Duration(2.4*float64(time.Second)) {
			t.Fatalf("Delay(1) = %v, want 2s plus at most 20%%", got)
		}
	}
}

func TestDoPreservesCompletedFailureWhenCancellationRacesTheResult(t *testing.T) {
	for _, message := range []string{"push rejected (non-fast-forward)", "i/o timeout"} {
		t.Run(message, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New(message)
			calls := 0
			value, err := Do(ctx, Options{}, func() (int, error) {
				calls++
				cancel()
				return 42, failure
			})
			if calls != 1 || value != 42 || !errors.Is(err, failure) || !errors.Is(err, context.Canceled) {
				t.Fatalf("value=%d calls=%d err=%v, want the completed failure and cancellation without retry", value, calls, err)
			}
		})
	}
}

func TestClassifyIgnoresEchoedURLsAndHonorsHTTPStatuses(t *testing.T) {
	for _, remote := range []string{
		"'https://github.com/org/timeout.git/'",
		"\"https://github.com/org/non-fast-forward.git/\"",
		"https://github.com/org/permission%20denied.git/",
		"git@github.com:org/non-fast-forward.git",
		"'https://github.com/org/permission denied.git/'",
	} {
		for _, diagnostic := range []struct {
			message string
			want    Class
		}{
			{"The requested URL returned error: 401", ""},
			{"The requested URL returned error: 403", ""},
			{"The requested URL returned error: 404", ""},
			{"The requested URL returned error: 422", ""},
			{"HTTP 403: Operation timed out", ""},
			{"HTTP/2 403: Operation timed out", ""},
			{"HTTP 401: secondary rate limit; Retry-After: 60", ""},
			{"HTTP 403: secondary rate limit; Retry-After: 60", ClassRateLimit},
			{"The requested URL returned error: 502", ClassHTTP5xx},
			{"Operation timed out", ClassTimeout},
			{"Timeout", ClassTimeout},
			{"invalid URL", ""},
		} {
			message := "fatal: unable to access " + remote + ": " + diagnostic.message
			if got := ClassifyText(message); got != diagnostic.want {
				t.Errorf("ClassifyText(%q)=%q, want %q", message, got, diagnostic.want)
			}
		}
	}
}

func TestPushTransportFailureWithGenericFooterRetries(t *testing.T) {
	for _, detail := range []string{
		"fatal: connection reset by peer",
		"fatal: unable to access 'https://github.com/test/repo.git': Operation timed out",
	} {
		t.Run(detail, func(t *testing.T) {
			calls := 0
			value, err := Do(context.Background(), Options{Base: time.Millisecond, Max: time.Millisecond}, func() (string, error) {
				calls++
				if calls < 3 {
					return "", errors.New(detail + "\nerror: failed to push some refs to 'https://github.com/test/repo.git'")
				}
				return "published", nil
			})
			if err != nil || calls != 3 || value != "published" {
				t.Fatalf("calls=%d value=%q err=%v, want recovery after the transport failure", calls, value, err)
			}
		})
	}
	for _, detail := range []string{"! [rejected] feature -> feature (non-fast-forward)", "remote: Permission denied", "remote: error: protected branch"} {
		t.Run(detail, func(t *testing.T) {
			calls := 0
			_, err := Do(context.Background(), Options{Base: time.Millisecond}, func() (string, error) {
				calls++
				return "", errors.New(detail + "\nfatal: connection reset by peer\nerror: failed to push some refs")
			})
			if err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v, want the rejection on the first attempt", calls, err)
			}
		})
	}
}
