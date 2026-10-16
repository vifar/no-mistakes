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

// ErrRequiredChecksUnavailable reports that a provider cannot supply a complete
// required-check set, for example because access is denied or a merge rule
// cannot be mapped to check contexts. Callers fall back to evaluating every check.
var ErrRequiredChecksUnavailable = errors.New("required status checks are not available for this branch")

// RequiredCheck is one status check a target branch requires before merging.
// Context is the provider's own check name, matched exactly against
// Check.Name. AppID, when positive, binds the requirement to the application
// that must produce the check: a same-named check from any other producer
// (including a legacy commit status, which has no application) does not
// satisfy it. Zero means any producer, except for SelfStatus below.
type RequiredCheck struct {
	Context string `json:"context"`
	AppID   int64  `json:"app_id,omitempty"`
	// SelfStatus identifies the provider's pipeline-owned status context.
	// Same-named check runs still count; only an unbound requirement may
	// omit a missing-check placeholder to avoid waiting on the pipeline itself.
	SelfStatus bool `json:"self_status,omitempty"`
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
// A readable configuration with no required status checks returns an empty set
// without error. An explicitly absent protection source can contribute an empty
// set while other sources still apply. An implementation returns
// ErrRequiredChecksUnavailable (possibly wrapped) when it cannot supply a complete
// set, and any other error when the read itself failed.
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
	// Resolved, when set, is the required set a scope already resolved under
	// this policy. A scope built from it does not read the provider again, so
	// a recorded gate is judged by the scope that raised it even when a later
	// read would resolve differently (for example after a transient failure
	// that made the monitor fall back to every check).
	Resolved *CheckScopeResolution `json:"resolved,omitempty"`
}

// CheckScopeResolution is the outcome of resolving a policy's required set:
// Derived is true when Required is authoritative; false means every check is
// evaluated.
type CheckScopeResolution struct {
	Required []RequiredCheck `json:"required,omitempty"`
	Derived  bool            `json:"derived,omitempty"`
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
// receipt. The first conclusive required-context read is cached per CheckScope,
// so repeated polls reuse it; an interrupted read leaves the cache unresolved
// and can be retried.
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
	s := &CheckScope{policy: policy, host: host, branch: strings.TrimSpace(branch), logf: logf}
	if policy.Resolved != nil {
		s.resolved = true
		s.required = normalizeRequired(policy.Resolved.Required)
		s.derived = policy.Resolved.Derived && len(s.required) > 0
	}
	return s
}

// Resolution returns the required set this scope resolved, and false while
// no conclusive read has happened yet.
func (s *CheckScope) Resolution() (CheckScopeResolution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resolved {
		return CheckScopeResolution{}, false
	}
	return CheckScopeResolution{Required: append([]RequiredCheck(nil), s.required...), Derived: s.derived}, true
}

// Policy returns the policy this scope applies, carrying its resolved
// required set once one is known, so a recorded copy rebuilds the same scope.
func (s *CheckScope) Policy() CheckScopePolicy {
	policy := s.policy
	if resolution, ok := s.Resolution(); ok {
		policy.Resolved = &resolution
	}
	return policy
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

// IncludesContext scopes retained work when no live check identifies its
// producer. An unknown producer must not discard an app-bound required repair;
// this name-only decision does not satisfy the requirement for readiness.
// External exclusions still win, and the raw context is tested before treating
// " / " as a workflow separator because it can be part of the provider's name.
func (s *CheckScope) IncludesContext(ctx context.Context, pr *PR, name string) bool {
	required, derived := s.requiredContexts(ctx, pr)
	required = append([]RequiredCheck(nil), required...)
	for i := range required {
		required[i].AppID = 0
		required[i].SelfStatus = false
	}
	identity := Check{Name: name}
	qualifiedIdentity := identity
	if workflow, job, qualified := strings.Cut(name, " / "); qualified {
		qualifiedIdentity.Name, qualifiedIdentity.Workflow = job, workflow
	}
	if matchesCheckIdentity(s.policy.ExternalChecks, identity) || matchesCheckIdentity(s.policy.ExternalChecks, qualifiedIdentity) {
		return false
	}
	if s.classify(identity, required, derived) == CheckRoleRequired {
		return true
	}
	for _, pattern := range s.policy.MustPass {
		if strings.TrimSpace(pattern) == name {
			return true
		}
	}
	return s.classify(qualifiedIdentity, required, derived) == CheckRoleRequired
}

func (s *CheckScope) partition(pr *PR, checks []Check, required []RequiredCheck, derived bool) ScopedChecks {
	// An empty observation is ready only with a known required set or trusted
	// no_ci, and only once something was observed - except when the target
	// requires nothing but the pipeline's own unbound status once external
	// requirements, which are never evaluated, are set aside. Collection
	// removes that status from the rollup and the run itself publishes it,
	// so waiting for an observed check would wait on the pipeline itself.
	var evaluated []RequiredCheck
	for _, want := range required {
		placeholder := Check{Name: want.Context, AppID: want.AppID, Kind: CheckKindRun}
		if s.classify(placeholder, required, derived) != CheckRoleExternal {
			evaluated = append(evaluated, want)
		}
	}
	observed := len(checks) > 0 || derived && selfStatusOnly(evaluated)
	out := ScopedChecks{emptyReady: observed && (derived || s.policy.NoCI), reviewBotCommentsAlways: s.policy.ReviewBotComments == "always"}
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
			if s.satisfies(c, want, "", required, derived) {
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
			if s.satisfies(c, RequiredCheck{Context: missing.Name}, missing.Workflow, required, derived) {
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

// selfStatusOnly reports whether required names only the pipeline's own
// status, unbound to an application.
func selfStatusOnly(required []RequiredCheck) bool {
	for _, r := range required {
		if !r.SelfStatus || r.AppID > 0 {
			return false
		}
	}
	return len(required) > 0
}

// classify reports a single check's role under an already-resolved scope.
func (s *CheckScope) classify(c Check, required []RequiredCheck, derived bool) CheckRole {
	role := s.classifyIdentity(c, required, derived)
	if role == CheckRoleRequired {
		return role
	}
	for _, identity := range c.Supersedes {
		switch s.classify(identity, required, derived) {
		case CheckRoleRequired:
			return CheckRoleRequired
		case CheckRoleInformational:
			role = CheckRoleInformational
		}
	}
	return role
}

func (s *CheckScope) classifyIdentity(c Check, required []RequiredCheck, derived bool) CheckRole {
	if matchesCheckIdentity(s.policy.ExternalChecks, c) {
		return CheckRoleExternal
	}
	if matchesCheckIdentity(s.policy.MustPass, c) {
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

func (s *CheckScope) satisfies(c Check, want RequiredCheck, workflow string, required []RequiredCheck, derived bool) bool {
	// A green workflow-level result reports the run, not its jobs: it cannot
	// stand in for a job that shares its name, which stays missing until it
	// reports. A failing one still blocks through its own identity.
	if c.WorkflowRun && (c.Bucket == CheckBucketPass || c.Bucket == CheckBucketSkip) {
		return false
	}
	if s.classifyIdentity(c, required, derived) == CheckRoleRequired && want.satisfiedBy(c) && (workflow == "" || c.Workflow == workflow) {
		return true
	}
	for _, identity := range c.Supersedes {
		if s.satisfies(identity, want, workflow, required, derived) {
			return true
		}
	}
	return false
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
