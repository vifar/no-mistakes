package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FallbackReason is why one attempt in a fallback chain may hand its turn to
// the next agent. It is the classification the fallback wrapper announces in
// its agent-fallback line and, for the pipeline, the vocabulary an attempt
// seam reports back with.
//
// The zero value means the attempt's failure is the agent's verdict on the
// work, and must not be re-run under a different agent.
type FallbackReason string

const (
	// FallbackNone is not fallback-eligible: the failure is a verdict.
	FallbackNone FallbackReason = ""
	// FallbackUnavailable means the agent never got as far as holding the
	// prompt (see isAgentUnavailableError).
	FallbackUnavailable FallbackReason = "unavailable"
	// FallbackRefused means the provider blocked the turn's content. See
	// IsRefused.
	FallbackRefused FallbackReason = "refused"
	// FallbackStalled means the attempt stopped advancing its turn without
	// producing anything, so there is nothing to preserve and another agent
	// may start over. Only the pipeline's attempt seam can establish this: it
	// owns the progress measurement and checks the worktree for side effects.
	FallbackStalled FallbackReason = "stalled"
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
		var (
			result *Result
			err    error
			reason FallbackReason
		)
		if opts.RunAttempt != nil {
			// The caller owns this invocation's bounds, so it runs the
			// attempt and tells us both the attempt's own context and whether
			// a failure may hand the turn on. That is the only way a stall can
			// fall back: the stalled attempt consumed a context this wrapper
			// does not own, and a fresh one has to be derived from the
			// invocation's parent.
			result, err, reason = opts.RunAttempt(current, currentOpts)
		} else {
			result, err = current.Run(ctx, currentOpts)
			reason = FallbackReasonFor(err)
		}
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
		// The caller may give each candidate its own bound through RunAttempt,
		// but all candidates still share this parent context. Once the parent
		// has expired or been cancelled the next candidate cannot start
		// meaningfully. Preserve the current adapter's report for the caller's
		// timeout diagnostic, but do not announce or instrument an attempt that
		// will inherit an already-dead context.
		if context.Cause(ctx) != nil {
			return nil, err
		}
		if i == len(candidates)-1 || reason == FallbackNone {
			return nil, err
		}
		next := candidates[i+1]
		emitAgentFallback(opts, current.Name(), next.Name(), reason, err)
	}
	return nil, lastErr
}

// ReplayUnsafe reports whether a failure's own record says its turn may not be
// run again: it provably ran a tool, or nothing available proves it did not.
// The pipeline's attempt seam reads it before classifying a stall or a budget
// cut too, because an adapter that lost its stream can hold a tool event it
// never delivered.
func ReplayUnsafe(err error) bool {
	return replayRanTools(err) || opencodeReplayUnsafe(err)
}

// FallbackReasonFor classifies a failed attempt by the reason class the
// fallback wrapper announces, for callers that run candidates themselves
// through RunOpts.RunAttempt.
//
// It answers only what the failure itself says. Whether the attempt left
// something behind - an edit, a commit - is the caller's to check: the pipeline
// attempt seam withdraws both FallbackRefused and FallbackStalled when the step
// worktree moved.
func FallbackReasonFor(err error) FallbackReason {
	if err == nil || IsStructuredOutputRejected(err) {
		return FallbackNone
	}
	// The adapter's own replay evidence outranks every class. A turn that
	// provably ran tools already has side effects on disk, in a commit, or in a
	// posted comment, and a second agent's fresh turn would do them twice. A
	// turn whose tool activity cannot be verified is the same answer in the
	// other direction - silence is not a proof - so both stay a verdict.
	//
	// It is read before the refusal class because a provider can refuse a turn
	// after a tool has run, and before the unavailable class because the
	// killed-process report a timeout appends ("codex exited: signal: killed")
	// would otherwise read as an agent that never started.
	if ReplayUnsafe(err) {
		return FallbackNone
	}
	if IsRefused(err) {
		return FallbackRefused
	}
	if isAgentUnavailableError(err) {
		return FallbackUnavailable
	}
	return FallbackNone
}

// DrivesAttempts reports whether a honours RunOpts.RunAttempt on every route,
// so a caller may install per-attempt bounds and leave the whole-invocation
// bound off. Fallback chains answer true; reviewAgents also answers true when
// any route reaches a chain and runs its single-agent routes through the seam
// itself. A caller that gets false must bound the invocation itself, because
// nothing would call RunAttempt.
func DrivesAttempts(a Agent) bool {
	driver, ok := a.(attemptDriver)
	return ok && driver.drivesAttempts()
}

type attemptDriver interface {
	drivesAttempts() bool
}

// drivesAttempts runs every candidate through RunOpts.RunAttempt when the
// caller installs one.
func (a *fallbackAgent) drivesAttempts() bool { return true }

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
func isAgentUnavailableError(err error) bool {
	if err == nil || IsStructuredOutputRejected(err) || opencodeReplayUnsafe(err) {
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

func fallbackDetail(err error) string {
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
