package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFallbackAgent_FallsBackOnSessionCreateFailure is the regression for
// issue #112: an opencode session-create failure (the wrong HTTP API, a
// rejected auth, a 5xx before any work started) is an agent that is
// UNAVAILABLE, so the next agent in the list must take over. Before the fix
// the run died on the first agent even though codex and claude were
// configured behind it.
func TestFallbackAgent_FallsBackOnSessionCreateFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "http 405 on the wrong api version",
			err:  fmt.Errorf("opencode create session: POST http://127.0.0.1:60526/session failed with 405: "),
		},
		{
			name: "http 500 from the agent server",
			err:  fmt.Errorf("opencode create session: POST http://127.0.0.1:60526/session failed with 500: boom"),
		},
		{
			name: "http 401 rejected credentials",
			err:  fmt.Errorf("opencode create session: POST http://127.0.0.1:60526/api/session failed with 401: Authentication required"),
		},
		{
			name: "http 404 unknown route",
			err:  fmt.Errorf("opencode create session: POST http://127.0.0.1:60526/api/session failed with 404: "),
		},
		{
			name: "connection refused reaching the agent server",
			err: fmt.Errorf("opencode create session: %w", &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &fallbackTestAgent{
				name: "opencode",
				run: func() (*Result, error) {
					return nil, agentSetupFailure(tc.err)
				},
			}
			second := &fallbackTestAgent{
				name: "codex",
				run: func() (*Result, error) {
					return &Result{Text: "ok"}, nil
				},
			}
			var chunks []string

			result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{
				OnChunk: func(text string) { chunks = append(chunks, text) },
			})
			if err != nil {
				t.Fatalf("Run() error = %v, want the fallback to take over", err)
			}
			if result == nil || result.Text != "ok" {
				t.Fatalf("Run() result = %+v, want text ok", result)
			}
			if first.calls != 1 || second.calls != 1 {
				t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
			}
			joined := strings.Join(chunks, "\n")
			if !strings.Contains(joined, "falling back to codex") {
				t.Fatalf("fallback log missing, got %q", joined)
			}
		})
	}
}

// TestFallbackAgent_DoesNotFallBackOnAWorkedTurn keeps the rule that a
// failure AFTER the agent did work is the agent's own verdict, not an
// unavailability: a findings result and a structured-output error must both
// stay on the first agent.
func TestFallbackAgent_DoesNotFallBackOnAWorkedTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "quoted setup error in structured output",
			err:  rejectStructuredOutput(errors.New("opencode create session: HTTP 405; agent start: failed")),
		},
		{
			name: "unsafe turn containing setup text",
			err:  opencodeTurnFailure(opencodeToolsRan, errors.New("opencode create session parse: agent exited: failed")),
		},
		{
			name: "structured output error after a real turn",
			err:  errors.New("opencode structured output failed after 2 internal retries: the model returned prose"),
		},
		{
			name: "a provider error after the turn ran tools",
			err:  errors.New("opencode ProviderAuthError: invalid api key (not retried: the failed turn already ran tools)"),
		},
		{
			name: "a turn failure that ran no tools is still a verdict",
			err:  errors.New("opencode APIError (status 400): the request was rejected as invalid"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &fallbackTestAgent{
				name: "opencode",
				run: func() (*Result, error) {
					return nil, tc.err
				},
			}
			second := &fallbackTestAgent{
				name: "codex",
				run: func() (*Result, error) {
					return &Result{Text: "should not run"}, nil
				},
			}

			_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
			if err == nil {
				t.Fatalf("Run() error = nil, want the first agent's own failure")
			}
			if first.calls != 1 || second.calls != 0 {
				t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
			}
		})
	}
}

// TestFallbackAgent_SessionCreateFailureOverHTTP exercises the real shape of
// the failure: a live HTTP server that answers the session-create request
// with a 405, the way OpenCode 2.x answered the 1.x route.
func TestFallbackAgent_SessionCreateFailureOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	first := &fallbackTestAgent{
		name: "opencode",
		run: func() (*Result, error) {
			a := &opencodeAgent{}
			_, err := a.createSession(context.Background(), server.URL, t.TempDir())
			if err != nil {
				return nil, err
			}
			return &Result{Text: "should not run"}, nil
		},
	}
	second := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return &Result{Text: "ok"}, nil
		},
	}

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run() error = %v, want the fallback to take over", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want text ok", result)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
	}
}

// TestFallbackAgent_ExpiredContextStillWinsOverTheNewRule keeps the
// bounded-context rule: a cancelled or expired context is never papered over
// by a fallback, whatever the failure looked like.
func TestFallbackAgent_ExpiredContextStillWinsOverTheNewRule(t *testing.T) {
	first := &fallbackTestAgent{
		name: "opencode",
		runCtx: func(ctx context.Context) (*Result, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("opencode create session: %w", ctx.Err())
		},
	}
	second := &fallbackTestAgent{
		name: "codex",
		run: func() (*Result, error) {
			return &Result{Text: "must not start"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := NewFallback([]Agent{first, second}).Run(ctx, RunOpts{})
	if err == nil {
		t.Fatal("Run() error = nil, want the deadline error")
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
	}
}
