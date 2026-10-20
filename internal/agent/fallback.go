package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type fallbackAgent struct {
	agents []Agent
}

// NewFallback returns an Agent that tries each agent in order when an
// invocation fails because the current agent process is unavailable.
func NewFallback(agents []Agent) Agent {
	switch len(agents) {
	case 0:
		return nil
	case 1:
		return agents[0]
	default:
		copied := make([]Agent, len(agents))
		copy(copied, agents)
		return &fallbackAgent{agents: copied}
	}
}

func (a *fallbackAgent) Name() string {
	if len(a.agents) == 0 {
		return ""
	}
	return a.agents[0].Name()
}

func (a *fallbackAgent) SupportsSessionResume() bool {
	for _, current := range a.agents {
		if SupportsSessionResume(current) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) SupportsSessionProvider(provider string) bool {
	for _, current := range a.agents {
		if SupportsSessionProvider(current, provider) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions fails closed over the whole fallback set: the
// wrapper may invoke any member, so it neutralizes the target repo's project
// agent-instruction files only if EVERY member does. A single unverified member
// makes the wrapper report false so the gate is refused rather than risk that
// member running unneutralized.
func (a *fallbackAgent) NeutralizesGateInstructions() bool {
	if len(a.agents) == 0 {
		return false
	}
	for _, current := range a.agents {
		if !NeutralizesGateInstructions(current) {
			return false
		}
	}
	return true
}

func (a *fallbackAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	candidates := a.agents
	if opts.Session != nil && opts.Session.ID != "" && opts.Session.Agent != "" {
		candidates = nil
		for _, current := range a.agents {
			if SupportsSessionProvider(current, opts.Session.Agent) {
				candidates = append(candidates, current)
				break
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("session provider %q is not configured", opts.Session.Agent)
		}
	}
	var lastErr error
	for i, current := range candidates {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		currentOpts := opts
		if currentOpts.Session != nil && currentOpts.Session.ID == "" && !SupportsSessionResume(current) {
			currentOpts.Session = nil
			currentOpts.SessionFallback = false
		}
		startedAt := time.Now()
		result, err := current.Run(ctx, currentOpts)
		if !ReportsAgentAttempts(current) {
			emitAgentAttempt(currentOpts, current.Name(), result, err, startedAt, time.Now())
		}
		if err == nil {
			if result != nil && result.Provider == "" {
				result.Provider = current.Name()
			}
			return result, nil
		}
		lastErr = err
		// A fallback is another attempt inside this invocation's existing
		// bounded context, not a fresh timeout policy. Once that context has
		// expired or been cancelled, the next candidate cannot start
		// meaningfully. Preserve the current adapter's report for the caller's
		// timeout diagnostic, but do not announce or instrument an attempt that
		// will inherit an already-dead context.
		if context.Cause(ctx) != nil {
			return nil, err
		}
		if i == len(candidates)-1 || !isAgentUnavailableError(err) {
			return nil, err
		}
		next := candidates[i+1]
		emitAgentFallback(opts, current.Name(), next.Name(), err)
	}
	return nil, lastErr
}

func (a *fallbackAgent) Close() error {
	var errs []string
	for _, ag := range a.agents {
		if err := ag.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ag.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close fallback agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

// isAgentUnavailableError reports whether err means "this agent could not be
// reached or set up", so the next agent in the list must take over.
//
// Two classes count:
//
//   - The process never came up (a missing binary, a server that exited
//     before becoming healthy). These are the historical cases.
//
//   - The agent's SERVER answered, but the request that sets the turn up
//     failed: creating a session, or reaching the server at all. This is the
//     issue #112 class. An HTTP 4xx/5xx at session create, or a connection
//     error, means the agent never held the prompt and never ran a tool, so
//     nothing was attempted and nothing was half-done. Treating it as a
//     verdict killed every run whose first agent was opencode against
//     OpenCode 2.x (which serves the API under /api and answered the 1.x
//     route with 405), even though codex and claude were configured behind it.
//
// What does NOT count: a failure after the agent's own turn ran - a real
// findings result, a structured-output rejection, a provider error on a
// completed turn. Those are the agent's verdict on the work, and re-running
// them under a different agent would discard a real answer.
//
// The line is drawn by WHERE the failure happened, not by how it reads. A
// setup failure is raised before the prompt is sent, so the call site tags it
// (agentSetupFailure). The text patterns below are the fallback for an
// adapter that cannot tag its own failure; they are deliberately narrow, and
// each names a step that happens before any work: creating the session, or
// reaching the server at all.
func isAgentUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errAgentSetupFailure) {
		return true
	}
	msg := strings.ToLower(err.Error())
	unavailable := []string{
		" start:",
		"start server ",
		" server: start server ",
		" exited:",
		" reported exit code ",
		// Issue #112: the session-create request is the last thing that
		// happens before the prompt is sent, so a failure there is a setup
		// failure. The HTTP status that follows is the agent server's own
		// verdict on the request.
		"create session:",
		"create session parse:",
	}
	for _, needle := range unavailable {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// errAgentSetupFailure marks a failure that happened while the agent was
// still being set up - before it held the prompt and before any tool could
// have run. It is the machine-readable form of "this agent is unavailable",
// and it is what lets the fallback wrapper tell a setup failure apart from
// the agent's own verdict on the work.
//
// The marker travels with the error rather than being re-derived from its
// text, because the same HTTP failure is reported by adapters in different
// wordings and only the call site knows whether the prompt had been sent.
var errAgentSetupFailure = errors.New("agent setup failed before the turn started")

// agentSetupFailure tags err as a setup failure: the agent never got as far
// as holding the prompt.
//
// The cause keeps its own message, so the error text a run reports and the
// agent_invocations failure category are unchanged by the tagging.
func agentSetupFailure(err error) error {
	if err == nil {
		return nil
	}
	return &agentSetupError{err: err}
}

// agentSetupError is the typed setup failure. It unwraps to the cause, so
// errors.Is and errors.As still see the original failure, and it reports the
// marker so errors.Is(err, errAgentSetupFailure) finds it.
type agentSetupError struct {
	err error
}

func (e *agentSetupError) Error() string { return e.err.Error() }

func (e *agentSetupError) Unwrap() error { return e.err }

func (e *agentSetupError) Is(target error) bool { return target == errAgentSetupFailure }

func fallbackReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 160
	if len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}
