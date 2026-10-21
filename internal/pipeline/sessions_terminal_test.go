package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type sessionRecoveryProbe struct {
	agent.Agent
	envelope string
	calls    []sessionCall
	results  []*agent.Result
}

func (p *sessionRecoveryProbe) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(p.Agent)
}
func (p *sessionRecoveryProbe) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(p.Agent, provider)
}
func (p *sessionRecoveryProbe) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	p.calls = append(p.calls, sessionCall{prompt: opts.Prompt, session: opts.Session, fallback: opts.SessionFallback})
	result, err := p.Agent.Run(ctx, opts)
	p.results = append(p.results, result)
	if err != nil {
		switch p.envelope {
		case "wrapped":
			err = fmt.Errorf("resume: %w", err)
		case "joined":
			err = errors.Join(nonTerminalSessionDiagnostic{errors.New("additional diagnostic")}, err)
		}
	}
	return result, err
}

type nonTerminalSessionDiagnostic struct{ error }

func (e nonTerminalSessionDiagnostic) Unwrap() error                { return e.error }
func (nonTerminalSessionDiagnostic) TerminalProviderError() bool    { return false }
func (nonTerminalSessionDiagnostic) UsageLimitReached() bool        { return false }
func (nonTerminalSessionDiagnostic) StructuredOutputRejected() bool { return false }

func TestRunSessions_NativeTerminalVerdictsOutrankResumeRetryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local Claude fixture uses POSIX shell")
	}
	for _, role := range []SessionRole{SessionRoleReviewer, SessionRoleFixer} {
		for _, verdict := range []struct {
			name, stream        string
			schema, limit, safe bool
		}{
			{"schema", `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true}`, true, false, false},
			{"usage", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}
{"type":"result","subtype":"error_during_execution","is_error":true}`, false, true, false},
			{"usage without result", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, false, true, false},
			{"ordinary resume failure", "", false, false, true},
		} {
			for _, code := range []int{0, 1} {
				for _, envelope := range []string{"direct", "wrapped", "joined"} {
					t.Run(string(role)+"/"+verdict.name+"/exit="+strconv.Itoa(code)+"/"+envelope, func(t *testing.T) {
						database, run := sessionTestDB(t)
						scratch := t.TempDir()
						quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
						count := filepath.Join(scratch, "calls")
						first := filepath.Join(scratch, "started")
						failed := filepath.Join(scratch, "failed")
						bin := filepath.Join(scratch, "claude")
						script := "#!/bin/sh\ncat >/dev/null\nprintf 'call\\n' >> " + quote(count) + "\n" +
							"if [ ! -e " + quote(first) + " ]; then\n: > " + quote(first) + "\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"held-session\",\"structured_output\":{}}'\n" +
							"elif [ ! -e " + quote(failed) + " ]; then\n: > " + quote(failed) + "\nprintf '%s\\n' " + quote(verdict.stream) + "\nexit " + strconv.Itoa(code) + "\n" +
							"else\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"replacement-session\",\"structured_output\":{}}'\nfi\n"
						if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
							t.Fatal(err)
						}
						claude, err := agent.New(types.AgentClaude, bin, nil)
						if err != nil {
							t.Fatal(err)
						}
						successor := newFakeSessionAgent()
						successor.name = "successor"
						chain := agent.NewFallback([]agent.Agent{claude, successor})
						t.Cleanup(func() { chain.Close() })
						probe := &sessionRecoveryProbe{Agent: chain, envelope: envelope}
						sessions := NewRunSessions(database, run.ID, probe, true)
						var events []agent.LifecycleEvent
						var logs []string
						opts := agent.RunOpts{Prompt: "initial turn", CWD: scratch, OnLifecycle: func(e agent.LifecycleEvent) { events = append(events, e) }}
						initial, err := sessions.Run(context.Background(), probe, role, opts, func(s string) { logs = append(logs, s) })
						if err != nil || initial.SessionID != "held-session" {
							t.Fatalf("initial session was not saved: result=%+v err=%v", initial, err)
						}
						held := sessions.id(role)
						opts.Prompt = "next turn"
						result, failure := sessions.Run(context.Background(), probe, role, opts, func(s string) { logs = append(logs, s) })
						resumed := probe.results[1]
						if resumed == nil || resumed.ResumeRetryable == nil || !*resumed.ResumeRetryable {
							t.Fatalf("fixture did not exercise ResumeRetryable=true: %+v", resumed)
						}
						wantCalls, wantID, wantFallbacks := 2, held.ID, 0
						if verdict.safe {
							wantCalls, wantID, wantFallbacks = 3, "replacement-session", 1
							if failure != nil || result == nil || result.SessionID != wantID {
								t.Fatalf("ordinary resume recovery failed: result=%+v err=%v", result, failure)
							}
						} else if failure == nil || !agent.IsTerminalProviderError(failure) || agent.IsUsageLimitReached(failure) != verdict.limit || errors.Is(failure, agent.ErrStructuredOutputRetriesExhausted) != verdict.schema {
							t.Fatalf("terminal evidence lost: %v", failure)
						}
						calls, err := os.ReadFile(count)
						if err != nil || len(strings.Fields(string(calls))) != wantCalls || len(probe.calls) != wantCalls || len(successor.calls) != 0 {
							t.Fatalf("unexpected replay: native=%q chain=%d successor=%d err=%v", calls, len(probe.calls), len(successor.calls), err)
						}
						if probe.calls[1].session == nil || probe.calls[1].session.ID != held.ID || probe.calls[1].fallback {
							t.Fatalf("second turn did not resume the saved session: %+v", probe.calls[1])
						}
						fallbackEvents := 0
						for _, event := range events {
							if event.Phase == agent.LifecyclePhaseFallback || event.Phase == agent.LifecyclePhaseQuotaSwitch {
								fallbackEvents++
							}
						}
						if fallbackEvents != wantFallbacks || len(logs) != wantFallbacks {
							t.Fatalf("unexpected fallback notification: events=%d logs=%d", fallbackEvents, len(logs))
						}
						if got := sessions.id(role); got.ID != wantID || got.Agent != held.Agent {
							t.Fatalf("in-memory identity lost: %+v", got)
						}
						stored, err := database.GetRunAgentSessions(run.ID)
						if err != nil || len(stored) != 1 || stored[0].Role != string(role) || stored[0].Agent != held.Agent || stored[0].SessionID != wantID {
							t.Fatalf("durable identity lost: rows=%+v err=%v", stored, err)
						}
					})
				}
			}
		}
	}
}

type guardedResumeAgent struct {
	*fakeSessionAgent
	failure error
}

func (a *guardedResumeAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.Session != nil && opts.Session.ID != "" {
		a.mu.Lock()
		a.calls = append(a.calls, sessionCall{prompt: opts.Prompt, session: opts.Session, fallback: opts.SessionFallback})
		a.mu.Unlock()
		retryable := true
		return &agent.Result{ResumeRetryable: &retryable}, a.failure
	}
	return a.fakeSessionAgent.Run(ctx, opts)
}

func TestRunSessions_ReplayEvidenceOutranksResumeRetryable(t *testing.T) {
	for _, role := range []SessionRole{SessionRoleReviewer, SessionRoleFixer} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"tools ran", agent.ErrOpencodeToolsAlreadyRan},
			{"unknown tools", agent.ErrOpencodeToolActivityUnknown},
			{"structured rejection", agent.RejectStructuredOutput(errors.New("invalid final schema"))},
			{"native schema exhaustion", agent.ErrStructuredOutputRetriesExhausted},
			{"usage marker", usageLimitReachedError{}},
			{"terminal marker", terminalProviderError{}},
		} {
			for _, envelope := range []string{"direct", "wrapped", "joined"} {
				t.Run(string(role)+"/"+failure.name+"/"+envelope, func(t *testing.T) {
					database, run := sessionTestDB(t)
					primary := &guardedResumeAgent{fakeSessionAgent: newFakeSessionAgent(), failure: failure.err}
					successor := newFakeSessionAgent()
					successor.name = "successor"
					probe := &sessionRecoveryProbe{Agent: agent.NewFallback([]agent.Agent{primary, successor}), envelope: envelope}
					sessions := NewRunSessions(database, run.ID, probe, true)
					if _, err := sessions.Run(context.Background(), probe, role, agent.RunOpts{Prompt: "initial"}, nil); err != nil {
						t.Fatal(err)
					}
					held := sessions.id(role)
					notifications := 0
					_, err := sessions.Run(context.Background(), probe, role, agent.RunOpts{Prompt: "resume", OnLifecycle: func(agent.LifecycleEvent) { notifications++ }}, func(string) { notifications++ })
					if !errors.Is(err, failure.err) || len(primary.calls) != 2 || len(successor.calls) != 0 || len(probe.calls) != 2 || notifications != 0 {
						t.Fatalf("protected turn replayed: error=%v primary=%d successor=%d chain=%d notifications=%d", err, len(primary.calls), len(successor.calls), len(probe.calls), notifications)
					}
					if got := sessions.id(role); got != held {
						t.Fatalf("protected failure forgot identity: got=%+v held=%+v", got, held)
					}
					rows, err := database.GetRunAgentSessions(run.ID)
					if err != nil || len(rows) != 1 || rows[0].SessionID != held.ID || rows[0].Agent != held.Agent || rows[0].Role != string(role) {
						t.Fatalf("protected failure deleted durable identity: rows=%+v err=%v", rows, err)
					}
				})
			}
		}
	}
}
