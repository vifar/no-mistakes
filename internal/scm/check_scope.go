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
	// when the provider reports them, and every check otherwise, subject to
	// configured must-pass additions and external exclusions.
	RequiredChecksAuto = "auto"
	// RequiredChecksAll evaluates every check on the head except configured
	// external contexts, regardless of the target branch's required set.
	RequiredChecksAll = "all"
)

// ErrRequiredChecksUnavailable reports that a provider has no readable
// required-check configuration for a branch: no protection or rules, or no
// permission to read them. Callers fall back to evaluating every check.
var ErrRequiredChecksUnavailable = errors.New("required status checks are not available for this branch")

// RequiredCheck is one status check a target branch requires before merging.
// Context is the provider's own check name, matched exactly against
// Check.Name. AppID, when positive, binds the requirement to the application
// that must produce the check: a same-named check from any other producer
// (including a legacy commit status, which has no application) does not
// satisfy it. Zero means any producer, except for SelfStatus below.
type RequiredCheck struct {
	Context string
	AppID   int64
	// SelfStatus identifies the provider's pipeline-owned status context.
	// Same-named check runs still count; only an unbound requirement may
	// omit a missing-check placeholder to avoid waiting on the pipeline itself.
	SelfStatus bool
}

func (r RequiredCheck) String() string {
	if r.AppID > 0 {
		return fmt.Sprintf("%s (app %d)", r.Context, r.AppID)
	}
	return r.Context
}

// satisfiedBy reports whether c is the check this requirement names.
func (r RequiredCheck) satisfiedBy(c Check) bool {
	return c.Name == r.Context && (r.AppID <= 0 || c.AppID == r.AppID) && (!r.SelfStatus || c.Kind == CheckKindRun)
}

// RequiredChecksReader is implemented by providers that can name the status
// checks a target branch requires before merging.
//
// An implementation returns ErrRequiredChecksUnavailable (possibly wrapped)
// when the branch has no readable required-check configuration, and any other
// error when the read itself failed.
type RequiredChecksReader interface {
	GetRequiredChecks(ctx context.Context, pr *PR, branch string) ([]RequiredCheck, error)
}

// CheckScopePolicy selects which checks on a PR head decide CI readiness and
// repair. It is plain data so a run can record the policy its CI step used and
// a later reader (the strict readiness receipt) can apply the same one.
type CheckScopePolicy struct {
	NoCI bool `json:"no_ci,omitempty"`
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
	emptyReady    bool
}

func (s ScopedChecks) Ready() bool {
	if len(s.Relevant) == 0 {
		return s.emptyReady
	}
	for _, check := range s.Relevant {
		if check.Bucket != CheckBucketPass && check.Bucket != CheckBucketSkip {
			return false
		}
	}
	return true
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
	required []RequiredCheck
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
func (s *CheckScope) requiredContexts(ctx context.Context, pr *PR) ([]RequiredCheck, bool) {
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
	contexts, err := reader.GetRequiredChecks(ctx, pr, s.branch)
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
		s.required = normalizeRequired(contexts)
		s.derived = len(s.required) > 0
		labels := make([]string, len(s.required))
		for i, r := range s.required {
			labels[i] = r.String()
		}
		if s.derived {
			s.log(fmt.Sprintf("CI scope: %q requires %d status check(s): %s", s.branch, len(s.required), strings.Join(labels, ", ")))
		} else {
			s.log(fmt.Sprintf("CI scope: %q has no required status checks; evaluating all checks", s.branch))
		}
	}
	s.resolved = true
	return s.required, s.derived
}

func normalizeRequired(required []RequiredCheck) []RequiredCheck {
	seen := make(map[RequiredCheck]bool, len(required))
	out := make([]RequiredCheck, 0, len(required))
	for _, r := range required {
		r.Context = strings.TrimSpace(r.Context)
		if r.AppID < 0 {
			r.AppID = 0
		}
		if r.Context == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Context != out[j].Context {
			return out[i].Context < out[j].Context
		}
		return out[i].AppID < out[j].AppID
	})
	return out
}

// Apply partitions checks. Precedence is external_checks, then must_pass, then
// the required set. Missing authoritative required contexts and literal
// must_pass contexts become pending placeholders unless excluded as external,
// so a missing required check blocks readiness exactly as a pending one does.
func (s *CheckScope) Apply(ctx context.Context, pr *PR, checks []Check) ScopedChecks {
	required, derived := s.requiredContexts(ctx, pr)
	return s.partition(pr, checks, required, derived)
}

func (s *CheckScope) Includes(ctx context.Context, pr *PR, check Check) bool {
	required, derived := s.requiredContexts(ctx, pr)
	return s.classify(check, required, derived) == CheckRoleRequired
}

func (s *CheckScope) partition(pr *PR, checks []Check, required []RequiredCheck, derived bool) ScopedChecks {
	out := ScopedChecks{emptyReady: len(checks) > 0 && (derived || s.policy.NoCI)}
	reportedForms := make(map[string]bool, len(checks)*2)
	for _, c := range checks {
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
	expected := append([]RequiredCheck(nil), required...)
	for _, p := range s.policy.MustPass {
		if p = strings.TrimSpace(p); p != "" && !strings.ContainsAny(p, "*?") && !reportedForms[p] && !matchesMissingContext([]string{p}, required) {
			expected = append(expected, RequiredCheck{Context: p})
		}
	}
	head := ""
	if pr != nil {
		head = pr.HeadSHA
	}
	seen := make(map[RequiredCheck]bool, len(expected))
	for _, want := range expected {
		if seen[want] || MatchesMissingContext(s.policy.ExternalChecks, want.Context) {
			continue
		}
		seen[want] = true
		satisfied := false
		for _, c := range checks {
			if want.satisfiedBy(c) {
				satisfied = true
				break
			}
		}
		if satisfied || (want.SelfStatus && want.AppID <= 0) {
			continue
		}
		out.Relevant = append(out.Relevant, Check{Name: want.Context, AppID: want.AppID, HeadSHA: head, Bucket: CheckBucketPending, State: "EXPECTED", Kind: CheckKindStatus, ExpectedMissing: true})
	}
	return out
}

// classify reports a single check's role under an already-resolved scope.
func (s *CheckScope) classify(c Check, required []RequiredCheck, derived bool) CheckRole {
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
		if r.satisfiedBy(c) {
			return CheckRoleRequired
		}
	}
	return CheckRoleInformational
}

// MatchesMissingContext reports whether a pattern names a required context
// that no check has registered yet. A missing context carries no workflow
// metadata, so besides the whole pattern, a pattern of the form
// "<workflow> / <job-pattern>" is tested by its job part against the bare
// context: the registered check would display as "<workflow> / <context>", and
// the pattern that excludes it must exclude its placeholder too.
func MatchesMissingContext(patterns []string, context string) bool {
	for _, p := range patterns {
		if MatchCheckPattern(p, context) {
			return true
		}
		if _, job, ok := strings.Cut(strings.TrimSpace(p), " / "); ok && MatchCheckPattern(job, context) {
			return true
		}
	}
	return false
}

// matchesMissingContext reports whether any pattern names one of the
// already-required contexts, so a literal must_pass entry for it does not add
// a second placeholder.
func matchesMissingContext(patterns []string, required []RequiredCheck) bool {
	for _, r := range required {
		if MatchesMissingContext(patterns, r.Context) {
			return true
		}
	}
	return false
}

// ExternalReviewBotChecks returns presence markers for registered review bots
// whose known check names match an external pattern, whether or not the bot
// has registered a check on the head. They let the absent-bot fallback honor
// the exclusion.
func (s *CheckScope) ExternalReviewBotChecks() []Check {
	var out []Check
	for _, bot := range ReviewBots {
		for _, name := range bot.CheckNames {
			if MatchesAnyCheckPattern(s.policy.ExternalChecks, name) {
				out = append(out, Check{Name: name, App: bot.AppSlug})
				break
			}
		}
	}
	return out
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
