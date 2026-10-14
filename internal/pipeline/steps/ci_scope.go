package steps

import (
	"encoding/json"
	"fmt"

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
	return sctx.Config.CI.CheckScopePolicy()
}

// newCIChecksScope builds the scope one CI step execution applies to every
// poll. The scope caches the target branch's required contexts, so the
// provider is asked once per execution rather than once per poll.
func newCIChecksScope(sctx *pipeline.StepContext, host scm.Host, targetBranch string) *scm.CheckScope {
	return scm.NewCheckScope(host, ciCheckScopePolicy(sctx), targetBranch, func(msg string) {
		if sctx.Log != nil {
			sctx.Log(msg)
		}
	})
}

// recordCIChecksScope stores the policy this execution evaluates checks under,
// so the strict ci-ready receipt, which re-reads live checks in another
// process, applies exactly the same one.
func recordCIChecksScope(sctx *pipeline.StepContext) error {
	encoded, err := json.Marshal(ciCheckScopePolicy(sctx))
	if err != nil {
		return fmt.Errorf("encode CI check scope: %w", err)
	}
	if err := sctx.DB.SetRunCheckScope(sctx.Run.ID, string(encoded)); err != nil {
		return fmt.Errorf("record CI check scope: %w", err)
	}
	return nil
}

func retainedCICheckIncluded(sctx *pipeline.StepContext, scope *scm.CheckScope, pr *scm.PR, checks []scm.Check, name, id string) bool {
	matched := false
	for _, check := range checks {
		if id != "" && check.ProviderID != id || id == "" && check.Name != name && check.DisplayName() != name {
			continue
		}
		matched = true
		if scope.Includes(sctx.Ctx, pr, check) {
			return true
		}
	}
	if !matched && id != "" {
		return retainedCICheckIncluded(sctx, scope, pr, checks, name, "")
	}
	return !matched && scope.Includes(sctx.Ctx, pr, scm.Check{Name: name})
}

func scopeRetainedCIFindings(sctx *pipeline.StepContext, scope *scm.CheckScope, pr *scm.PR, checks []scm.Check) error {
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
