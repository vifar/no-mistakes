package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciCheckScopePolicy is the configured check-scope policy for this run. A run
// without resolved configuration evaluates every check.
func ciCheckScopePolicy(sctx *pipeline.StepContext) scm.CheckScopePolicy {
	if sctx == nil || sctx.Config == nil {
		return scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll}
	}
	policy := sctx.Config.CI.CheckScopePolicy()
	policy.NoCI = sctx.Config.NoCI
	return policy
}

// newCIChecksScope builds a scope for one target branch. CheckScope owns the
// required-context cache; the monitor replaces the scope when the target changes.
func newCIChecksScope(sctx *pipeline.StepContext, host scm.Host, targetBranch string) *scm.CheckScope {
	return scm.NewCheckScope(host, ciCheckScopePolicy(sctx), targetBranch, func(msg string) {
		if sctx.Log != nil {
			sctx.Log(msg)
		}
	})
}

// recordedCIChecksScope returns the policy the CI step recorded when it last
// observed this run. ok is false when nothing was recorded.
func recordedCIChecksScope(sctx *pipeline.StepContext) (policy scm.CheckScopePolicy, ok bool, err error) {
	raw, err := sctx.DB.GetRunCheckScope(sctx.Run.ID)
	if err != nil {
		return policy, false, fmt.Errorf("read recorded CI check scope: %w", err)
	}
	if raw == "" {
		return policy, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, false, fmt.Errorf("invalid recorded CI check scope")
	}
	return policy, true, nil
}

// recordCIChecksScope stores the policy this execution evaluates checks under,
// so the strict ci-ready receipt, which re-reads live checks in another
// process, applies exactly the same one. Once scope has resolved its required
// set, the record carries it too: approval verification and the receipt then
// judge a gate by the scope that raised it rather than by a fresh read, which
// could resolve differently after a transient failure.
func recordCIChecksScope(sctx *pipeline.StepContext, scope *scm.CheckScope) error {
	policy := ciCheckScopePolicy(sctx)
	if scope != nil {
		policy = scope.Policy()
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode CI check scope: %w", err)
	}
	if err := sctx.DB.SetRunCheckScope(sctx.Run.ID, string(encoded)); err != nil {
		return fmt.Errorf("record CI check scope: %w", err)
	}
	return nil
}

// retainedCICheckIncluded reports whether retained work (a selected finding or
// an outstanding rerun) still names an in-scope check. A selection naming an
// observed check itself is judged by that check's role, including the jobs it
// supersedes; a selection naming a job that a workflow-level check now stands
// for is judged as that job alone, so an excluded job never reaches the fixer
// because a sibling it was superseded with is required.
func retainedCICheckIncluded(sctx *pipeline.StepContext, scope *scm.CheckScope, pr *scm.PR, checks []scm.Check, name, id string) bool {
	matched := false
	for _, check := range checks {
		if id != "" && check.ProviderID != id {
			continue
		}
		if name == "" || check.Name == name || check.DisplayName() == name {
			matched = true
			if scope.Includes(sctx.Ctx, pr, check) {
				return true
			}
			continue
		}
		identities := supersededIdentitiesNamed(check, name)
		if len(identities) == 0 && id != "" {
			// The ID names this check, under a name it no longer carries.
			identities = []scm.Check{check}
		}
		for _, identity := range identities {
			matched = true
			if scope.Includes(sctx.Ctx, pr, identity) {
				return true
			}
		}
	}
	if !matched && id != "" {
		// The provider ID can name a job a workflow-level check now stands
		// for; it identifies that job exactly, even when the job shares its
		// workflow's name, so judge it alone before falling back to names.
		for _, check := range checks {
			if identity, ok := supersededIdentityWithID(check, id); ok {
				return scope.Includes(sctx.Ctx, pr, identity)
			}
		}
		return retainedCICheckIncluded(sctx, scope, pr, checks, name, "")
	}
	return !matched && scope.IncludesContext(sctx.Ctx, pr, name)
}

// supersededIdentityWithID finds the job identity with provider ID id that
// check stands for, without the identities it superseded in turn.
func supersededIdentityWithID(check scm.Check, id string) (scm.Check, bool) {
	for _, identity := range check.Supersedes {
		if identity.ProviderID == id {
			identity.Supersedes = nil
			return identity, true
		}
		if found, ok := supersededIdentityWithID(identity, id); ok {
			return found, true
		}
	}
	return scm.Check{}, false
}

// supersededIdentitiesNamed returns the job identities check stands for that
// carry name, each without the identities it superseded in turn.
func supersededIdentitiesNamed(check scm.Check, name string) []scm.Check {
	var identities []scm.Check
	for _, identity := range check.Supersedes {
		if identity.Name == name || identity.DisplayName() == name {
			own := identity
			own.Supersedes = nil
			identities = append(identities, own)
		}
		identities = append(identities, supersededIdentitiesNamed(identity, name)...)
	}
	return identities
}

func scopeRetainedCIFindings(sctx *pipeline.StepContext, scope *scm.CheckScope, pr *scm.PR, checks []scm.Check) error {
	scoped := scope.Apply(sctx.Ctx, pr, checks)
	excludedApps := map[string]bool{}
	excluded := append(scoped.External, scope.ExternalReviewBotChecks()...)
	if !reviewBotCommentsAlways(sctx) {
		excluded = append(excluded, scoped.Informational...)
	}
	for _, check := range excluded {
		if check.App != "" {
			excludedApps[strings.ToLower(strings.TrimSpace(check.App))] = true
		}
	}
	if !reviewBotCommentsAlways(sctx) {
		for _, check := range scoped.Relevant {
			delete(excludedApps, strings.ToLower(strings.TrimSpace(check.App)))
		}
	}
	for _, encoded := range []*string{&sctx.PreviousFindings, &sctx.DeferredFindings} {
		if *encoded == "" {
			continue
		}
		findings, err := types.ParseFindingsJSON(*encoded)
		if err != nil {
			return fmt.Errorf("scope retained CI findings: %w", err)
		}
		kept := findings.Items[:0]
		for _, item := range findings.Items {
			if item.Category == types.FindingCategoryCIReviewBot {
				if excludedApps[strings.ToLower(strings.TrimSpace(item.CheckApp))] {
					continue
				}
				if reviewBotCommentsAlways(sctx) {
					kept = append(kept, item)
					continue
				}
			}
			if item.Check == "" || retainedCICheckIncluded(sctx, scope, pr, checks, item.Check, item.CheckID) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(findings.Items) {
			continue
		}
		findings.Items = kept
		findings.Summary = ""
		if len(kept) == 0 {
			*encoded = ""
			continue
		}
		data, err := json.Marshal(findings)
		if err != nil {
			return err
		}
		*encoded = string(data)
	}
	return nil
}

func (s *CIStep) scopeRetainedCIReruns(sctx *pipeline.StepContext, scope *scm.CheckScope, pr *scm.PR, relevant, excluded []scm.Check) error {
	candidate := checkRerunBudget{spent: s.transientReruns.spent, rollup: make(map[string]rerunRollupState)}
	checks := append(append([]scm.Check(nil), relevant...), excluded...)
	for name, state := range s.transientReruns.rollup {
		if retainedCICheckIncluded(sctx, scope, pr, checks, name, "") {
			candidate.rollup[name] = state
		}
	}
	if len(candidate.rollup) == len(s.transientReruns.rollup) {
		return nil
	}
	if err := s.persistRerunBudgetCandidate(sctx, &candidate); err != nil {
		return fmt.Errorf("persist scoped CI reruns: %w", err)
	}
	s.transientReruns = candidate
	return nil
}
