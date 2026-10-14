package scm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type requiredChecksHost struct {
	Host
	contexts []string
	err      error
	calls    int
}

func (h *requiredChecksHost) GetRequiredCheckContexts(context.Context, *PR, string) ([]string, error) {
	h.calls++
	return h.contexts, h.err
}

func names(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Name)
	}
	return out
}

func approvalChecks() []Check {
	return []Check{
		{Name: "build", Bucket: CheckBucketPass},
		{Name: "approve", Workflow: "Final Approval", Bucket: CheckBucketFail},
		{Name: "approval-status", Kind: CheckKindStatus, Bucket: CheckBucketFail},
		{Name: "extra", Bucket: CheckBucketFail},
	}
}

func TestMatchCheckPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"approve", "approve", true},
		{"approve", "approve (x)", false},
		{"approve*", "approve (x)", true},
		{"*/ approve*", "Final Approval / approve (a)", true},
		{"Final Approval / approve*", "Final Approval / approve (a/b)", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"*", "anything", true},
		{"*build*", "x build y", true},
		{"Build", "build", false},
		{"", "build", false},
	} {
		if got := MatchCheckPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchCheckPattern(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestCheckScopeRequiredSetMakesOtherChecksInformational(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build"}}
	scoped := NewCheckScope(host, CheckScopePolicy{}, "main", nil).Apply(context.Background(), &PR{HeadSHA: "h"}, approvalChecks())
	if got := names(scoped.Relevant); !reflect.DeepEqual(got, []string{"build"}) {
		t.Fatalf("relevant = %v, want [build]", got)
	}
	if got := names(scoped.Informational); !reflect.DeepEqual(got, []string{"approve", "approval-status", "extra"}) {
		t.Fatalf("informational = %v", got)
	}
	if len(scoped.External) != 0 {
		t.Fatalf("external = %v, want none", names(scoped.External))
	}
}

func TestCheckScopePrecedenceExternalOverMustPassOverRequired(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build", "approve", "approval-status"}}
	policy := CheckScopePolicy{
		MustPass:       []string{"extra", "approve"},
		ExternalChecks: []string{"Final Approval / approve*", "approval-status"},
	}
	scoped := NewCheckScope(host, policy, "main", nil).Apply(context.Background(), &PR{HeadSHA: "h"}, approvalChecks())
	if got := names(scoped.External); !reflect.DeepEqual(got, []string{"approve", "approval-status"}) {
		t.Fatalf("external = %v: external_checks must beat must_pass and the required set", got)
	}
	if got := names(scoped.Relevant); !reflect.DeepEqual(got, []string{"build", "extra"}) {
		t.Fatalf("relevant = %v, want build (required) and extra (must_pass)", got)
	}
}

func TestCheckScopeMissingRequiredContextBlocksAsPending(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build", "deploy"}}
	policy := CheckScopePolicy{MustPass: []string{"lint", "slow-*"}, ExternalChecks: []string{"deploy"}}
	scoped := NewCheckScope(host, policy, "main", nil).Apply(context.Background(), &PR{HeadSHA: "h"}, []Check{{Name: "build", Bucket: CheckBucketPass, HeadSHA: "h"}})
	if got := names(scoped.Relevant); !reflect.DeepEqual(got, []string{"build", "lint"}) {
		t.Fatalf("relevant = %v, want build plus an expected placeholder for literal must_pass lint only", got)
	}
	placeholder := scoped.Relevant[1]
	if placeholder.Bucket != CheckBucketPending || placeholder.HeadSHA != "h" || !placeholder.Pending() {
		t.Fatalf("placeholder = %+v, want pending on the head", placeholder)
	}
}

func TestCheckScopeFallsBackToAllChecks(t *testing.T) {
	t.Parallel()
	for name, host := range map[string]*requiredChecksHost{
		"unavailable":       {err: ErrRequiredChecksUnavailable},
		"read error":        {err: errors.New("boom")},
		"no required check": {contexts: nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs []string
			scoped := NewCheckScope(host, CheckScopePolicy{ExternalChecks: []string{"approval-status"}}, "main", func(m string) { logs = append(logs, m) }).
				Apply(context.Background(), &PR{HeadSHA: "h"}, approvalChecks())
			if got := names(scoped.Relevant); !reflect.DeepEqual(got, []string{"build", "approve", "extra"}) {
				t.Fatalf("relevant = %v, want every check but the external one", got)
			}
			if len(scoped.Informational) != 0 || len(logs) != 1 || !strings.Contains(logs[0], "evaluating all checks") {
				t.Fatalf("informational = %v, logs = %v", names(scoped.Informational), logs)
			}
		})
	}
}

func TestCheckScopeAllModeNeverAsksTheProvider(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build"}}
	scoped := NewCheckScope(host, CheckScopePolicy{RequiredChecks: RequiredChecksAll}, "main", nil).Apply(context.Background(), &PR{}, approvalChecks())
	if host.calls != 0 || len(scoped.Relevant) != 4 {
		t.Fatalf("calls = %d, relevant = %v", host.calls, names(scoped.Relevant))
	}
}

func TestCheckScopeReadsRequiredContextsOncePerScope(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build"}}
	scope := NewCheckScope(host, CheckScopePolicy{}, "main", nil)
	for range 5 {
		scope.Apply(context.Background(), &PR{HeadSHA: "h"}, approvalChecks())
	}
	if host.calls != 1 || scope.ProviderFetches() != 1 {
		t.Fatalf("provider reads = %d (scope reports %d), want 1 for five polls", host.calls, scope.ProviderFetches())
	}
	failing := &requiredChecksHost{err: ErrRequiredChecksUnavailable}
	scope = NewCheckScope(failing, CheckScopePolicy{}, "main", nil)
	for range 3 {
		scope.Apply(context.Background(), &PR{}, approvalChecks())
	}
	if failing.calls != 1 {
		t.Fatalf("an unavailable answer is cached too: provider reads = %d, want 1", failing.calls)
	}
}

func TestCheckScopeDoesNotCacheAnInterruptedRead(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{err: context.Canceled}
	scope := NewCheckScope(host, CheckScopePolicy{}, "main", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scope.Apply(ctx, &PR{}, approvalChecks())
	host.err, host.contexts = nil, []string{"build"}
	scoped := scope.Apply(context.Background(), &PR{}, approvalChecks())
	if host.calls != 2 || len(scoped.Relevant) != 1 {
		t.Fatalf("provider reads = %d, relevant = %v; an interrupted read must be retried", host.calls, names(scoped.Relevant))
	}
}

func TestCheckScopeProvidersWithoutAReaderEvaluateAllChecks(t *testing.T) {
	t.Parallel()
	scoped := NewCheckScope(nil, CheckScopePolicy{ExternalChecks: []string{"approve*"}}, "main", nil).Apply(context.Background(), &PR{}, approvalChecks())
	if got := names(scoped.Relevant); !reflect.DeepEqual(got, []string{"build", "approval-status", "extra"}) {
		t.Fatalf("relevant = %v", got)
	}
	if got := names(scoped.External); !reflect.DeepEqual(got, []string{"approve"}) {
		t.Fatalf("external = %v", got)
	}
}

func TestScopedChecksSummaryNamesUnsettledOnly(t *testing.T) {
	t.Parallel()
	host := &requiredChecksHost{contexts: []string{"build"}}
	scoped := NewCheckScope(host, CheckScopePolicy{ExternalChecks: []string{"approve"}}, "main", nil).Apply(context.Background(), &PR{}, append(approvalChecks(), Check{Name: "docs", Bucket: CheckBucketPass}))
	got := scoped.Summary()
	want := "external (awaiting outside approval): approve (fail); informational (not required, ignored): approval-status (fail), extra (fail)"
	if got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}
