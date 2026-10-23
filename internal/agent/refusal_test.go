package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A provider content-policy refusal is not a verdict on the work: the same
// prompt usually goes through on another provider. Issue #201 measured a review
// that died on opencode's refusal with claude configured third and never tried.
func TestFallbackAgentFallsBackOnContentFilterRefusal(t *testing.T) {
	first := &fallbackTestAgent{
		name: "opencode",
		run: func() (*Result, error) {
			return nil, errors.New("opencode session.execution.failed: provider.content-filter: Provider blocked the response")
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "ok"}, nil
		},
	}
	var chunks []string

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result == nil || result.Text != "ok" {
		t.Fatalf("Run() result = %+v, want the second agent's answer", result)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = first %d second %d, want 1/1", first.calls, second.calls)
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "agent opencode failed (refused:") || !strings.Contains(joined, "falling back to claude") {
		t.Fatalf("fallback log missing the refused class, got %q", joined)
	}
}

// The refusal class is not a licence to replay a turn that already ran tools:
// its side effects are on disk, in a commit, or in a posted comment.
func TestFallbackAgentDoesNotFallBackOnRefusalAfterToolsRan(t *testing.T) {
	refused := errors.New("opencode session.execution.failed: provider.content-filter: Provider blocked the response")
	first := &fallbackTestAgent{
		name: "opencode",
		run: func() (*Result, error) {
			return nil, MarkRefused(errors.Join(refused, errOpencodeToolsAlreadyRan))
		},
	}
	second := &fallbackTestAgent{
		name: "claude",
		run: func() (*Result, error) {
			return &Result{Text: "must not run"}, nil
		},
	}

	_, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if err == nil {
		t.Fatal("Run() error = nil, want the refusal surfaced")
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("calls = first %d second %d, want 1/0", first.calls, second.calls)
	}
}

// The classification table the operator sees in the fallback line.
func TestFallbackReasonForClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want FallbackReason
	}{
		{
			name: "process never came up",
			err:  errors.New(`codex start: exec: "codex": executable file not found`),
			want: FallbackUnavailable,
		},
		{
			name: "usage limit on exit",
			err:  errors.New("codex exited: exit status 1: You've hit your usage limit"),
			want: FallbackUnavailable,
		},
		{
			name: "opencode provider content filter",
			err:  errors.New("opencode session.execution.failed: provider.content-filter: Provider blocked the response"),
			want: FallbackRefused,
		},
		{
			name: "native cli content policy wording",
			err:  errors.New("grok error: subtype=error_during_execution: request rejected by content policy"),
			want: FallbackRefused,
		},
		{
			name: "tagged refusal without the wording",
			err:  MarkRefused(errors.New("request blocked")),
			want: FallbackRefused,
		},
		{
			name: "ordinary failed turn",
			err:  errors.New("opencode message: the review could not be completed"),
			want: FallbackNone,
		},
		{
			name: "structured output rejection quoting a refusal",
			err:  rejectStructuredOutput(errors.New("blocked by content policy")),
			want: FallbackNone,
		},
		{
			name: "refusal after the turn ran tools",
			err:  MarkRefused(errors.Join(errors.New("provider.content-filter: blocked"), errOpencodeToolsAlreadyRan)),
			want: FallbackNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FallbackReasonFor(tc.err); got != tc.want {
				t.Fatalf("FallbackReasonFor(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// A caller installs its own per-attempt bounds only when every route reaches a
// chain that honours RunOpts.RunAttempt; otherwise the invocation would run
// with no bound at all.
func TestDrivesAttempts(t *testing.T) {
	single := &fallbackTestAgent{name: "codex"}
	chain := NewFallback([]Agent{
		&fallbackTestAgent{name: "codex"},
		&fallbackTestAgent{name: "claude"},
	})
	if DrivesAttempts(single) {
		t.Fatal("a single agent must not claim to drive attempts")
	}
	if !DrivesAttempts(chain) {
		t.Fatal("a fallback chain must drive attempts")
	}
	if !DrivesAttempts(WithSteering(chain, "/evidence")) {
		t.Fatal("steering must forward the chain's capability")
	}
	if DrivesAttempts(WithReviewRoles(single, ReviewRoles{Fixer: RoundedRole{Agent: chain}})) {
		t.Fatal("a mixed tree (single primary, chained fixer) must not claim to drive attempts")
	}
	if !DrivesAttempts(WithReviewRoles(chain, ReviewRoles{Fixer: RoundedRole{Agent: chain}})) {
		t.Fatal("an all-chain tree must drive attempts")
	}
}
