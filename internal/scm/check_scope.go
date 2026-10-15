package scm

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	if slices.Contains(c.Supersedes, r.Context) {
		return true
	}
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
	NoCI              bool   `json:"no_ci,omitempty"`
	ReviewBotComments string `json:"review_bot_comments,omitempty"`
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
	Relevant                []Check
	External                []Check
	Informational           []Check
	emptyReady              bool
	reviewBotCommentsAlways bool
}

func (s ScopedChecks) Settled() bool {
	for _, check := range s.Relevant {
		switch check.Bucket {
		case CheckBucketPass, CheckBucketFail, CheckBucketSkip:
		default:
			return false
		}
	}
	if s.reviewBotCommentsAlways {
		for _, check := range s.Informational {
			if _, ok := ReviewBotForCheck(check); ok && check.Pending() {
				return false
			}
		}
	}
	return true
}

func (s ScopedChecks) Ready() bool {
	if !s.Settled() {
		return false
	}
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
	out := ScopedChecks{emptyReady: len(checks) > 0 && (derived || s.policy.NoCI), reviewBotCommentsAlways: s.policy.ReviewBotComments == "always"}
	for _, c := range checks {
		switch s.classify(c, required, derived) {
		case CheckRoleExternal:
			out.External = append(out.External, c)
		case CheckRoleInformational:
			out.Informational = append(out.Informational, c)
		default:
			out.Relevant = append(out.Relevant, c)
		}
	}
	head := ""
	if pr != nil {
		head = pr.HeadSHA
	}
	for _, want := range required {
		missing := Check{Name: want.Context, AppID: want.AppID, Kind: CheckKindRun, HeadSHA: head, Bucket: CheckBucketPending, State: "EXPECTED", ExpectedMissing: true}
		if s.classify(missing, required, derived) != CheckRoleRequired || (want.SelfStatus && want.AppID <= 0) {
			continue
		}
		satisfied := false
		for _, c := range checks {
			if want.satisfiedBy(c) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			out.Relevant = append(out.Relevant, missing)
		}
	}
	for _, pattern := range s.policy.MustPass {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.ContainsAny(pattern, "*?") {
			continue
		}
		missing := Check{Name: pattern, HeadSHA: head, Bucket: CheckBucketPending, State: "EXPECTED", ExpectedMissing: true}
		if workflow, job, qualified := strings.Cut(pattern, " / "); qualified {
			missing.Name, missing.Workflow = job, workflow
		}
		if s.classify(missing, required, derived) != CheckRoleRequired {
			continue
		}
		satisfied := false
		for _, c := range out.Relevant {
			if slices.Contains(c.Supersedes, missing.Name) || c.Name == missing.Name && (missing.Workflow == "" || c.Workflow == missing.Workflow) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			out.Relevant = append(out.Relevant, missing)
		}
	}
	return out
}

// classify reports a single check's role under an already-resolved scope.
func (s *CheckScope) classify(c Check, required []RequiredCheck, derived bool) CheckRole {
	if matchesCheckIdentity(s.policy.ExternalChecks, c) {
		return CheckRoleExternal
	}
	if matchesCheckIdentity(s.policy.MustPass, c) {
		return CheckRoleRequired
	}
	for _, superseded := range c.Supersedes {
		if matchesCheckIdentity(s.policy.MustPass, Check{Name: superseded, Workflow: c.Name}) {
			return CheckRoleRequired
		}
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

func matchesCheckIdentity(patterns []string, check Check) bool {
	for _, pattern := range patterns {
		if workflow, job, qualified := strings.Cut(strings.TrimSpace(pattern), " / "); qualified {
			if (check.Workflow == "" || MatchCheckPattern(workflow, check.Workflow)) && MatchCheckPattern(job, check.Name) {
				return true
			}
		} else if MatchCheckPattern(pattern, check.Name) {
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
			if s.classify(Check{Name: name, App: bot.AppSlug}, nil, false) == CheckRoleExternal {
				out = append(out, Check{Name: name, App: bot.AppSlug})
				break
			}
		}
	}
	return out
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

func (scoped ScopedChecks) ReviewBotPresenceMarkers(externalBots []Check) []Check {
	markers := make([]Check, 0, len(scoped.External)+len(externalBots))
	markers = append(markers, scoped.External...)
	markers = append(markers, externalBots...)
	for _, check := range scoped.Informational {
		if _, ok := ReviewBotForCheck(check); !ok {
			continue
		}
		if !check.Pending() {
			continue
		}
		markers = append(markers, check)
	}
	return markers
}

func ReviewBotCommentChecks(checks []Check, excluded ...Check) []Check {
	var green []Check
	present := map[string]bool{}
	for _, check := range excluded {
		if bot, ok := ReviewBotForCheck(check); ok {
			present[bot.AppSlug] = true
		}
	}
	for _, check := range checks {
		bot, ok := ReviewBotForApp(check.App)
		if !ok {
			continue
		}
		present[bot.AppSlug] = true
		if check.Bucket == CheckBucketPass || check.Bucket == CheckBucketSkip {
			green = append(green, check)
		}
	}
	for _, bot := range ReviewBots {
		if !present[bot.AppSlug] {
			green = append(green, Check{App: bot.AppSlug})
		}
	}
	return green
}

func ReadReadinessReviewBotComments(ctx context.Context, host Host, pr *PR, policy CheckScopePolicy, checks []Check, excluded ...Check) ([]ReviewComment, error) {
	if policy.ReviewBotComments != "always" {
		return nil, nil
	}
	candidates := ReviewBotCommentChecks(checks, excluded...)
	if len(candidates) == 0 || !host.Capabilities().ReviewComments {
		return nil, nil
	}
	reader, ok := host.(ReviewCommentsHost)
	if !ok {
		return nil, ErrUnsupported
	}
	comments, err := reader.GetReviewComments(ctx, pr)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, check := range candidates {
		if bot, ok := ReviewBotForApp(check.App); ok {
			allowed[bot.AppSlug] = true
		}
	}
	var unresolved []ReviewComment
	for _, comment := range comments {
		if bot, ok := ReviewBotForLogin(host.Provider(), comment.Author); ok && allowed[bot.AppSlug] {
			unresolved = append(unresolved, comment)
		}
	}
	return unresolved, nil
}
