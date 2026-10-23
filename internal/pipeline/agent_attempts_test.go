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
