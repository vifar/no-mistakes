package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// This file covers the two fallback classes that a step could not reach before
// issue #201: a provider content-policy refusal, and an attempt that stalls
// having produced nothing. Both are decided here rather than in
// internal/agent/fallback.go because only this seam knows how an attempt ended
// (a stall is a measured absence of progress, not an error string) and whether
// the attempt left anything behind in the worktree.

// fallbackTestWorktree creates a git worktree with one commit, so the
// side-effect gate has a real HEAD and a real status to read.
func fallbackTestWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-q", "-m", "seed"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func attemptStepContext(workDir string, agents ...agent.Agent) *StepContext {
	return &StepContext{
		Ctx:           context.Background(),
		Agent:         agent.NewFallback(agents),
		AgentAttempts: true,
		WorkDir:       workDir,
		Config: &config.Config{
			// The wall clock is deliberately far away: only the progress bound
			// may end a silent attempt, so a pass cannot come from a timeout.
			AgentTimeout:      30 * time.Second,
			AgentStallTimeout: 60 * time.Millisecond,
		},
	}
}

func fallbackEvents(events []agent.LifecycleEvent) string {
	var lines []string
	for _, event := range events {
		if event.Phase == agent.LifecyclePhaseFallback {
			lines = append(lines, event.Message)
		}
	}
	return strings.Join(lines, "\n")
}

// The measured shape: the opencode fixer worked, went silent, and was killed at
// its invocation budget. Because the budget wrapped the whole fallback chain,
// the step failed with the remaining agents untried. The silent attempt must
// now hand the turn to the next agent, which gets its own fresh budget.
func TestRunAgent_FallsBackAfterASilentStallWithAFreshBudget(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	stalled := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	var freshContext bool
	next := &hangingAgent{
		name: "claude",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			// The second attempt really did get a fresh bound: its context is
			// live while it runs, not the spent one the failed attempt held.
			freshContext = ctx.Err() == nil
			return &agent.Result{Text: "reviewed"}, nil
		},
	}
	sctx := attemptStepContext(dir, stalled, next)

	var lifecycle []agent.LifecycleEvent
	result, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "review",
		OnLifecycle: func(event agent.LifecycleEvent) { lifecycle = append(lifecycle, event) },
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if result == nil || result.Text != "reviewed" {
		t.Fatalf("result = %+v, want the second agent's answer", result)
	}
	if stalled.calls != 1 || next.calls != 1 {
		t.Fatalf("calls = stalled %d next %d, want 1/1", stalled.calls, next.calls)
	}
	if log := fallbackEvents(lifecycle); !strings.Contains(log, "(stalled:") || !strings.Contains(log, "falling back to claude") {
		t.Fatalf("fallback log = %q, want the stalled class named", log)
	}
	if !freshContext {
		t.Fatal("the replacement attempt inherited the stalled attempt's spent context")
	}
}

// A stall after any progress is a turn that acted and then wedged. It may have
// edited files, so another agent must not start over on top of it.
func TestRunAgent_StallWithProgressDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	stalled := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnChunk("thinking about the diff")
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, stalled, next)

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	if !errors.Is(err, ErrAgentStall) {
		t.Fatalf("error = %v, want ErrAgentStall", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) after a stall that had progress", next.calls)
	}
}

// The same for edits: a stalled attempt that left the worktree dirty fails the
// step exactly as it did before, so a half-edited tree is never handed to a
// second agent.
func TestRunAgent_StallWithPartialEditsDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	stalled := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "fix.go"), []byte("half written\n"), 0o600); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, stalled, next)

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	if !errors.Is(err, ErrAgentStall) {
		t.Fatalf("error = %v, want ErrAgentStall", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) over a dirty worktree", next.calls)
	}
}

// A provider content-policy refusal is not a verdict on the work: the same
// prompt usually goes through on another provider. This is the second half of
// the measured failure, where claude was configured third and never tried.
func TestRunAgent_FallsBackAfterAContentFilterRefusal(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	refused := &hangingAgent{
		name: "opencode",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("opencode session.execution.failed: provider.content-filter: Provider blocked the response")
		},
	}
	next := &hangingAgent{
		name: "claude",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "reviewed"}, nil
		},
	}
	sctx := attemptStepContext(dir, refused, next)

	var lifecycle []agent.LifecycleEvent
	result, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "review",
		OnLifecycle: func(event agent.LifecycleEvent) { lifecycle = append(lifecycle, event) },
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if result == nil || result.Text != "reviewed" {
		t.Fatalf("result = %+v, want the second agent's answer", result)
	}
	if log := fallbackEvents(lifecycle); !strings.Contains(log, "(refused:") {
		t.Fatalf("fallback log = %q, want the refused class named", log)
	}
}

// A refusal from an attempt that already changed the worktree is not safe to
// hand on: the next agent would start over on top of edits or a commit it
// cannot attribute.
func TestRunAgent_RefusalWithChangedWorktreeDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	refused := &hangingAgent{
		name: "opencode",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "fix.go"), []byte("written\n"), 0o600); err != nil {
				return nil, err
			}
			return nil, errors.New("opencode session.execution.failed: provider.content-filter: Provider blocked the response")
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, refused, next)

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	if err == nil || !strings.Contains(err.Error(), "content-filter") {
		t.Fatalf("error = %v, want the refusal surfaced", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) after a refusal on a dirty worktree", next.calls)
	}
}

// An ordinary failed verdict keeps failing the step: the responsibility for the
// work stays with the agent that reached a conclusion about it.
func TestRunAgent_VerdictFailureDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	failed := &hangingAgent{
		name: "opencode",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("opencode message: the review could not be completed")
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, failed, next)

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	if err == nil {
		t.Fatal("a verdict failure must not be retried under another agent")
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) after a verdict failure", next.calls)
	}
}

// A cancelled run stops everything at once: no replacement attempt is announced
// or started on a context the operator (or the daemon) already cancelled.
func TestRunAgent_CancelledRunStartsNoReplacementAttempt(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	blocked := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, blocked, next)
	sctx.Config.AgentStallTimeout = 10 * time.Second

	parent, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel(errors.New("aborted by operator"))
	}()

	var lifecycle []agent.LifecycleEvent
	_, err := sctx.RunAgentContext(parent, agent.RunOpts{
		Prompt:      "review",
		OnLifecycle: func(event agent.LifecycleEvent) { lifecycle = append(lifecycle, event) },
	})
	if err == nil || !strings.Contains(err.Error(), "aborted by operator") {
		t.Fatalf("error = %v, want the operator's cancellation cause", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) after a cancelled run", next.calls)
	}
	if log := fallbackEvents(lifecycle); log != "" {
		t.Fatalf("fallback announced on a cancelled run: %q", log)
	}
}

// The total wall clock across fallbacks is bounded: a third candidate may not
// start once the invocation has spent twice its per-attempt budget.
func TestAgentAttempt_FallbackWallClockIsBounded(t *testing.T) {
	t.Parallel()
	spent := &agentAttempt{
		parent: context.Background(),
		invoke: func(context.Context, agent.Agent, agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("unreachable")
		},
		timeout:  time.Minute,
		started:  time.Now().Add(-3 * time.Minute),
		attempts: 2,
		lastErr:  errors.New("previous attempt failed"),
	}
	next := &hangingAgent{name: "claude"}
	_, err, reason := spent.run(next, agent.RunOpts{})
	if err == nil {
		t.Fatal("a spent fallback wall clock must stop the chain")
	}
	if reason != agent.FallbackNone {
		t.Fatalf("reason = %q, want no fallback announced", reason)
	}
	if next.calls != 0 {
		t.Fatalf("candidate ran %d time(s) with no wall clock left", next.calls)
	}
	if !strings.Contains(err.Error(), "previous attempt failed") {
		t.Fatalf("error = %q, want the last attempt's failure preserved", err)
	}
}

// A wall-clock budget can expire before the progress bound does (a review
// budget shorter than agent_stall_timeout, say). The evidence, not the adapter's
// text, decides: an attempt that produced nothing and left the tree untouched
// is the same class as a stall.
func TestRunAgent_FallsBackWhenTheBudgetExpiresWithNoOutput(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	silent := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	next := &hangingAgent{
		name: "claude",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "reviewed"}, nil
		},
	}
	sctx := attemptStepContext(dir, silent, next)
	sctx.Config.AgentTimeout = 80 * time.Millisecond
	// The progress bound is far away on purpose: only the wall clock can end
	// this attempt, which is the shape the review budget produces.
	sctx.Config.AgentStallTimeout = 10 * time.Second

	var lifecycle []agent.LifecycleEvent
	result, err := sctx.RunAgent(agent.RunOpts{
		Prompt:      "review",
		OnLifecycle: func(event agent.LifecycleEvent) { lifecycle = append(lifecycle, event) },
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if result == nil || result.Text != "reviewed" {
		t.Fatalf("result = %+v, want the second agent's answer", result)
	}
	if log := fallbackEvents(lifecycle); !strings.Contains(log, "(stalled:") {
		t.Fatalf("fallback log = %q, want the stalled class named", log)
	}
}

// A budget that expires while the turn was working is a sizing decision, not a
// dead agent: its partial work stays with the agent that made it.
func TestRunAgent_BudgetExpiryAfterProgressDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	working := &hangingAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			for {
				select {
				case <-ctx.Done():
					// A killed native agent reports this on the way out, which
					// is textually the agent-unavailable class.
					return nil, errors.New("codex exited: signal: killed")
				case <-time.After(5 * time.Millisecond):
					opts.OnChunk("working")
				}
			}
		},
	}
	next := &hangingAgent{name: "claude"}
	sctx := attemptStepContext(dir, working, next)
	sctx.Config.AgentTimeout = 80 * time.Millisecond
	// Far beyond twice the silent budget: the first attempt keeps the cap the
	// operator configured for it, because no fallback is owed its time.
	sctx.Config.AgentWorkingTimeout = 400 * time.Millisecond

	start := time.Now()
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	elapsed := time.Since(start)

	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want the invocation budget", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) after a budget cut with progress", next.calls)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("attempt ran %s, want the configured 400ms still-working cap", elapsed)
	}
}

// A fallback attempt is clamped to what is left of the invocation's total, so
// two spent candidates cannot hand a third a full budget.
func TestAgentAttempt_ClampsEachAttemptToTheRemainingTotal(t *testing.T) {
	t.Parallel()
	var seen time.Duration
	attempt := &agentAttempt{
		parent:  context.Background(),
		timeout: 100 * time.Millisecond,
		// The first candidate already ran, and 150ms of the 200ms total is
		// gone.
		attempts: 1,
		started:  time.Now().Add(-150 * time.Millisecond),
		invoke: func(ctx context.Context, _ agent.Agent, _ agent.RunOpts) (*agent.Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("attempt ran with no deadline")
			}
			seen = time.Until(deadline)
			return &agent.Result{Text: "ok"}, nil
		},
	}
	if _, err, _ := attempt.run(&hangingAgent{name: "claude"}, agent.RunOpts{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen > 60*time.Millisecond {
		t.Fatalf("attempt deadline = %s, want the ~50ms left of the total rather than a fresh 100ms", seen)
	}
}

// A provider that owns a stored role session keeps the turn: the chain must not
// start at a recovered primary and hand it a foreign session id, which would
// replace the identity the owner could still resume.
func TestRunAgentSession_StoredIdentityRoutesToItsOwner(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	database, run := sessionTestDB(t)
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "claude", "claude-sess"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	codex := newFakeSessionAgent()
	codex.name = "codex"
	claude := newFakeSessionAgent()
	claude.name = "claude"
	chain := agent.NewFallback([]agent.Agent{codex, claude})

	sctx := attemptStepContext(dir, codex, claude)
	sctx.Agent = chain
	sctx.Run = run
	sctx.Sessions = NewRunSessions(database, run.ID, chain, true)
	sctx.Config.AgentTimeout = 30 * time.Second

	if _, err := sctx.RunAgentSession(SessionRoleFixer, agent.RunOpts{Prompt: "fix"}); err != nil {
		t.Fatalf("RunAgentSession: %v", err)
	}
	if len(codex.calls) != 0 {
		t.Fatalf("codex calls = %d, want the stored owner to keep the turn", len(codex.calls))
	}
	if len(claude.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1", len(claude.calls))
	}
	if got := claude.calls[0].session; got == nil || got.ID != "claude-sess" {
		t.Fatalf("claude session = %+v, want its own stored identity resumed", got)
	}
	stored, err := database.GetRunAgentSessions(run.ID)
	if err != nil {
		t.Fatalf("stored sessions: %v", err)
	}
	if len(stored) != 1 || stored[0].Agent != "claude" || stored[0].SessionID != "claude-sess" {
		t.Fatalf("stored session = %+v, want claude's identity kept", stored)
	}
}

// A route with no candidate behind it (a single configured reviewer or fixer)
// keeps the caps the operator set for it: there is no fallback time to bound,
// so the fallback total must not shorten a three-hour still-working cap.
func TestAgentAttempt_SingleAttemptKeepsItsConfiguredWorkingCap(t *testing.T) {
	t.Parallel()
	var seen time.Duration
	attempt := &agentAttempt{
		parent:  context.Background(),
		timeout: 30 * time.Minute,
		working: 3 * time.Hour,
		started: time.Now(),
		invoke: func(ctx context.Context, _ agent.Agent, _ agent.RunOpts) (*agent.Result, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("attempt ran with no deadline")
			}
			seen = time.Until(deadline)
			return &agent.Result{Text: "ok"}, nil
		},
	}
	if _, err, _ := attempt.run(&hangingAgent{name: "claude"}, agent.RunOpts{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen < 2*time.Hour {
		t.Fatalf("attempt deadline = %s, want the configured 3h still-working cap", seen)
	}
}

// A refusal after reasoning only is safe to hand on: no tool ran, so there is no
// side effect to repeat. Counting every progress event as a tool would withhold
// that safe fallback.
func TestRunAgent_RefusalAfterReasoningOnlyStillFallsBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	refused := &hangingAgent{
		name: "grok",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "grok", Phase: agent.LifecyclePhaseProgress})
			return nil, errors.New("grok error: subtype=error_during_execution: request rejected by content policy")
		},
	}
	next := &hangingAgent{
		name: "claude",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "reviewed"}, nil
		},
	}
	sctx := attemptStepContext(dir, refused, next)

	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if next.calls != 1 {
		t.Fatalf("next agent calls = %d, want the refusal handed on", next.calls)
	}
}

// A tool call is not assistant output, so a tool-only turn reports it through
// LifecyclePhaseTool. That evidence must stop the stall class: the tool may have
// left a side effect a second agent would repeat.
func TestRunAgent_StallAfterToolActivityDoesNotFallBack(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	worked := &hangingAgent{
		name: "claude",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			opts.OnLifecycle(agent.LifecycleEvent{Agent: "claude", Phase: agent.LifecyclePhaseTool})
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	next := &hangingAgent{name: "codex"}
	sctx := attemptStepContext(dir, worked, next)

	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "review"})
	if !errors.Is(err, ErrAgentStall) {
		t.Fatalf("error = %v, want ErrAgentStall", err)
	}
	if next.calls != 0 {
		t.Fatalf("next agent ran %d time(s) over a turn that ran a tool", next.calls)
	}
}

// A fixer turn the invocation budget cut is not a failed resume: the session
// wrapper must not retry the same candidate cold over partial work.
func TestRunAgentSession_BoundedTurnIsNotRetriedCold(t *testing.T) {
	t.Parallel()
	dir := fallbackTestWorktree(t)
	database, run := sessionTestDB(t)
	if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "claude", "claude-sess"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	claude := newFakeSessionAgent()
	claude.name = "claude"
	claude.blockNext = true
	spare := newFakeSessionAgent()
	spare.name = "codex"
	// Two candidates so this is a real chain, and claude owns the session so
	// the chain starts there.
	chain := agent.NewFallback([]agent.Agent{spare, claude})

	sctx := attemptStepContext(dir, spare, claude)
	sctx.Agent = chain
	sctx.Run = run
	sctx.Sessions = NewRunSessions(database, run.ID, chain, true)
	sctx.Config.AgentTimeout = 80 * time.Millisecond
	sctx.Config.AgentStallTimeout = 10 * time.Second

	_, err := sctx.RunAgentSession(SessionRoleFixer, agent.RunOpts{Prompt: "fix"})
	if err == nil || !errors.Is(err, ErrAgentTimeout) {
		t.Fatalf("error = %v, want the invocation budget", err)
	}
	if len(claude.calls) != 1 {
		t.Fatalf("claude calls = %d, want the bounded turn to run once", len(claude.calls))
	}
	if len(spare.calls) != 0 {
		t.Fatalf("spare agent ran %d time(s) after a bounded turn", len(spare.calls))
	}
}
