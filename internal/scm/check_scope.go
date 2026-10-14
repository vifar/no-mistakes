package scm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Check-scope modes for CheckScopePolicy.RequiredChecks.
const (
	// RequiredChecksAuto evaluates the target branch's required status checks
	// when the provider reports them, and every check otherwise.
	RequiredChecksAuto = "auto"
	// RequiredChecksAll evaluates every check on the head, as the CI step did
	// before required-check scoping existed.
	RequiredChecksAll = "all"
)

// ErrRequiredChecksUnavailable reports that a provider has no readable
// required-check configuration for a branch: no protection or rules, or no
// permission to read them. Callers fall back to evaluating every check.
var ErrRequiredChecksUnavailable = errors.New("required status checks are not available for this branch")

// RequiredChecksReader is implemented by providers that can name the status
// check contexts a target branch requires before merging. Contexts are the
// provider's own check names, matched exactly against Check.Name.
//
// An implementation returns ErrRequiredChecksUnavailable (possibly wrapped)
// when the branch has no readable required-check configuration, and any other
// error when the read itself failed.
type RequiredChecksReader interface {
	GetRequiredCheckContexts(ctx context.Context, pr *PR, branch string) ([]string, error)
}

// CheckScopePolicy selects which checks on a PR head decide CI readiness and
// repair. It is plain data so a run can record the policy its CI step used and
// a later reader (the strict readiness receipt) can apply the same one.
type CheckScopePolicy struct {
	// RequiredChecks is RequiredChecksAuto (the default when empty) or
	// RequiredChecksAll.
	RequiredChecks string `json:"required_checks,omitempty"`
	// MustPass lists glob patterns for contexts that always count as required.
	MustPass []string `json:"must_pass,omitempty"`
	// ExternalChecks lists glob patterns for contexts that are never evaluated:
	// they pass only after an outside party acts (for example a GitHub App
	// that approves the exact head), so neither waiting on nor repairing them
	// is meaningful. It outranks MustPass and the required set.
	ExternalChecks []string `json:"external_checks,omitempty"`
}

// CheckRole is how a check participates in CI readiness.
type CheckRole string

const (
	// CheckRoleRequired checks decide readiness and are repaired when failing.
	CheckRoleRequired CheckRole = "required"
	// CheckRoleExternal checks await an outside party and are only reported.
	CheckRoleExternal CheckRole = "external"
	// CheckRoleInformational checks are not required by the target branch and
	// are only reported.
	CheckRoleInformational CheckRole = "informational"
)

// ScopedChecks is the partition of a head's checks under a CheckScope.
type ScopedChecks struct {
	// Relevant are the checks that decide readiness, including a synthetic
	// pending entry for each required context the provider has not reported.
	Relevant      []Check
	External      []Check
	Informational []Check
}

// Summary describes the checks that do not decide readiness and are not yet
// green, or "" when there is nothing to report. It is stable for an unchanged
// observation so callers can log it only when it changes.
func (s ScopedChecks) Summary() string {
	var parts []string
	if names := unsettledNames(s.External); len(names) > 0 {
		parts = append(parts, "external (awaiting outside approval): "+strings.Join(names, ", "))
	}
	if names := unsettledNames(s.Informational); len(names) > 0 {
		parts = append(parts, "informational (not required, ignored): "+strings.Join(names, ", "))
	}
	return strings.Join(parts, "; ")
}

func unsettledNames(checks []Check) []string {
	var names []string
	for _, c := range checks {
		if c.Bucket == CheckBucketPass || c.Bucket == CheckBucketSkip {
			continue
		}
		names = append(names, fmt.Sprintf("%s (%s)", c.Name, c.Bucket))
	}
	sort.Strings(names)
	return names
}

// CheckScope decides which of a head's checks are relevant to CI readiness.
// It is the single definition shared by the CI step and the strict readiness
// receipt. The target branch's required contexts are read at most once per
// CheckScope, so a monitor that polls repeatedly costs one extra provider
// read, not one per poll.
type CheckScope struct {
	policy CheckScopePolicy
	host   Host
	branch string
	logf   func(string)

	mu       sync.Mutex
	resolved bool
	required []string
	derived  bool
}

// NewCheckScope builds a scope for host and the PR's target branch. logf may
// be nil.
func NewCheckScope(host Host, policy CheckScopePolicy, branch string, logf func(string)) *CheckScope {
	return &CheckScope{policy: policy, host: host, branch: strings.TrimSpace(branch), logf: logf}
}

func (s *CheckScope) log(msg string) {
	if s.logf != nil {
		s.logf(msg)
	}
}

// requiredContexts returns the target branch's required contexts and whether
// they are authoritative. The first conclusive answer is cached.
func (s *CheckScope) requiredContexts(ctx context.Context, pr *PR) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved {
		return s.required, s.derived
	}
	mode := strings.ToLower(strings.TrimSpace(s.policy.RequiredChecks))
	if mode == RequiredChecksAll {
		s.resolved = true
		return nil, false
	}
	reader, ok := s.host.(RequiredChecksReader)
	if !ok || s.branch == "" {
		s.log("CI scope: provider has no required-check reader or target branch; evaluating all checks")
		s.resolved = true
		return nil, false
	}
	contexts, err := reader.GetRequiredCheckContexts(ctx, pr, s.branch)
	switch {
	case err != nil && ctx.Err() != nil:
		// An interrupted read says nothing about the branch; ask again later.
		return nil, false
	case errors.Is(err, ErrRequiredChecksUnavailable):
		s.log(fmt.Sprintf("CI scope: required checks of %q are not readable (%v); evaluating all checks", s.branch, err))
	case err != nil:
		s.log(fmt.Sprintf("CI scope: could not read required checks of %q: %v; evaluating all checks", s.branch, err))
	case len(contexts) == 0:
		s.log(fmt.Sprintf("CI scope: %q has no required status checks; evaluating all checks", s.branch))
	default:
		s.required = normalizeContexts(contexts)
		s.derived = true
		s.log(fmt.Sprintf("CI scope: %q requires %d status check(s): %s", s.branch, len(s.required), strings.Join(s.required, ", ")))
	}
	s.resolved = true
	return s.required, s.derived
}

func normalizeContexts(contexts []string) []string {
	seen := make(map[string]bool, len(contexts))
	out := make([]string, 0, len(contexts))
	for _, c := range contexts {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Apply partitions checks. Precedence is external_checks, then must_pass, then
// the required set. When the required set is authoritative, every required
// context (and every literal must_pass context) that no check reports yet is
// represented by a pending placeholder so a missing required check blocks
// readiness exactly as a pending one does.
func (s *CheckScope) Apply(ctx context.Context, pr *PR, checks []Check) ScopedChecks {
	required, derived := s.requiredContexts(ctx, pr)
	return s.partition(pr, checks, required, derived)
}

func (s *CheckScope) Includes(ctx context.Context, pr *PR, check Check) bool {
	required, derived := s.requiredContexts(ctx, pr)
	return s.classify(check, required, derived) == CheckRoleRequired
}

func (s *CheckScope) partition(pr *PR, checks []Check, required []string, derived bool) ScopedChecks {
	var out ScopedChecks
	reported := make(map[string]bool, len(checks))
	reportedForms := make(map[string]bool, len(checks)*2)
	for _, c := range checks {
		reported[c.Name] = true
		reportedForms[c.Name] = true
		reportedForms[c.DisplayName()] = true
		switch s.classify(c, required, derived) {
		case CheckRoleExternal:
			out.External = append(out.External, c)
		case CheckRoleInformational:
			out.Informational = append(out.Informational, c)
		default:
			out.Relevant = append(out.Relevant, c)
		}
	}
	expected := append([]string(nil), required...)
	for _, p := range s.policy.MustPass {
		if p = strings.TrimSpace(p); p != "" && !strings.ContainsAny(p, "*?") && !reportedForms[p] {
			expected = append(expected, p)
		}
	}
	head := ""
	if pr != nil {
		head = pr.HeadSHA
	}
	seen := make(map[string]bool, len(expected))
	for _, name := range expected {
		if reported[name] || seen[name] || MatchesAnyCheckPattern(s.policy.ExternalChecks, name) {
			continue
		}
		seen[name] = true
		out.Relevant = append(out.Relevant, Check{Name: name, HeadSHA: head, Bucket: CheckBucketPending, State: "EXPECTED", Kind: CheckKindStatus})
	}
	return out
}

// classify reports a single check's role under an already-resolved scope.
func (s *CheckScope) classify(c Check, required []string, derived bool) CheckRole {
	if MatchesAnyCheckPattern(s.policy.ExternalChecks, c.Name) || MatchesAnyCheckPattern(s.policy.ExternalChecks, c.DisplayName()) {
		return CheckRoleExternal
	}
	if MatchesAnyCheckPattern(s.policy.MustPass, c.Name) || MatchesAnyCheckPattern(s.policy.MustPass, c.DisplayName()) {
		return CheckRoleRequired
	}
	if !derived {
		return CheckRoleRequired
	}
	for _, r := range required {
		if r == c.Name {
			return CheckRoleRequired
		}
	}
	return CheckRoleInformational
}

// MatchesAnyCheckPattern reports whether name matches any glob pattern.
func MatchesAnyCheckPattern(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchCheckPattern(p, name) {
			return true
		}
	}
	return false
}

// MatchCheckPattern reports whether name matches a glob pattern in which '*'
// matches any run of characters (including '/') and '?' matches exactly one.
// Matching is case-sensitive and anchored.
func MatchCheckPattern(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || name == "" {
		return false
	}
	p, n := []rune(pattern), []rune(name)
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case star >= 0:
			mark++
			ni = mark
			pi = star + 1
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
