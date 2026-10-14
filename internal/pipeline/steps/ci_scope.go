package steps

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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
