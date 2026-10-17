package steps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type rewindObservation struct {
	target string
	source *string
}

func sameRewindSource(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func sameRewindIdentity(p *db.PRContext, c db.PRContextCandidate) bool {
	return p.PRURL == c.PRURL && p.SourceRepo == c.SourceRepo && p.SourceBranch == c.SourceBranch && p.TargetBranch == c.TargetBranch && p.ForgeHeadSHA == c.ForgeHeadSHA
}

// samePendingRewindIdentity validates the full discovery observation, even
// when a discovered PR cannot attach because its head differs from local HEAD.
func samePendingRewindIdentity(pending *db.TargetAdvanceRewind, previous *db.PRContext, candidate db.PRContextCandidate, selected pipeline.PRTargetSelection) bool {
	if previous == nil || !sameRewindIdentity(previous, candidate) {
		return false
	}
	if pending.ForgeHeadSHA == nil {
		return previous.PRURL == "" && selected.PRURL == "" && selected.ForgeHeadSHA == ""
	}
	return previous.PRURL != "" && selected.PRURL == previous.PRURL && selected.ForgeHeadSHA == *pending.ForgeHeadSHA
}

// observeRewindRemotes never reads tracking refs. Each operation has bounded
// network calls and owns its refs until its caller has finished integration.
func observeRewindRemotes(c *pipeline.StepContext, target string, generation int64) (observation rewindObservation, cleanup func() error, err error) {
	prefix := fmt.Sprintf("refs/no-mistakes/rewind/%s/%d/", c.Run.ID, generation)
	refs := []string{prefix + "target", prefix + "source"}
	cleanup = func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx), fetchUpstreamTimeout)
		defer cancel()
		var errs []error
		for _, ref := range refs {
			if _, e := git.Run(ctx, c.WorkDir, "update-ref", "--no-deref", "-d", ref); e != nil {
				errs = append(errs, e)
			}
		}
		return errors.Join(errs...)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()
	targetSHA, err := observeRewindBranch(c, resolveUpstreamURL(c), target, refs[0])
	if err != nil {
		return observation, cleanup, fmt.Errorf("observe rewind target: %w", err)
	}
	if targetSHA == nil {
		return observation, cleanup, fmt.Errorf("rewind target branch is absent")
	}
	observation.target = *targetSHA
	observation.source, err = observeRewindBranch(c, resolvePushURL(c), strings.TrimPrefix(c.Run.Branch, "refs/heads/"), refs[1])
	if err != nil {
		return observation, cleanup, fmt.Errorf("observe rewind source: %w", err)
	}
	return observation, cleanup, nil
}

func observeRewindBranch(c *pipeline.StepContext, remote, branch, privateRef string) (*string, error) {
	if branch == "" {
		return nil, fmt.Errorf("rewind branch is empty")
	}
	ctx, cancel := context.WithTimeout(c.Ctx, fetchUpstreamTimeout)
	out, err := git.Run(ctx, c.WorkDir, "ls-remote", "--exit-code", "--refs", remote, "refs/heads/"+branch)
	cancel()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return nil, nil
		}
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
		return nil, fmt.Errorf("unexpected remote branch observation")
	}
	ctx, cancel = context.WithTimeout(c.Ctx, fetchUpstreamTimeout)
	err = git.FetchRemoteBranchToPrivateRef(ctx, c.WorkDir, remote, branch, privateRef)
	cancel()
	if err != nil {
		return nil, err
	}
	sha, err := git.Run(c.Ctx, c.WorkDir, "rev-parse", "--verify", privateRef+"^{commit}")
	if err != nil {
		return nil, err
	}
	if sha != fields[0] {
		return nil, fmt.Errorf("remote branch moved during rewind observation")
	}
	return &sha, nil
}

func classifyTargetAdvance(c *pipeline.StepContext, step types.StepName, selection pipeline.PRTargetSelection, previous *db.PRContext, candidate db.PRContextCandidate, source *string) (db.PRContextBindResult, error) {
	refusal := func(cause error) (db.PRContextBindResult, error) {
		return db.PRContextBindResult{}, fmt.Errorf("PR comparison changed after rebase; start a new run for the current target and head: %w", cause)
	}
	if previous == nil || !sameRewindIdentity(previous, candidate) || candidate.LocalHeadSHA != c.Run.HeadSHA || candidate.LocalHeadSHA != previous.LocalHeadSHA || previous.TargetSHA == candidate.TargetSHA ||
		(selection.ForgeHeadSHA != "" && previous.PRURL == "") {
		return refusal(db.ErrTargetAdvanceIneligible)
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", previous.TargetSHA, candidate.TargetSHA); err != nil {
		return refusal(fmt.Errorf("target is not a strict fast-forward: %w", err))
	}
	maxRewinds := config.DefaultMaxTargetAdvanceRewinds
	if c.Config != nil {
		maxRewinds = c.Config.Rebase.MaxTargetAdvanceRewinds
	}
	result, err := c.DB.RewindRunPRContext(c.Run.ID, candidate, db.TargetAdvanceRewindRequest{
		ExpectedGeneration: previous.Generation, Step: step, MaxRewinds: maxRewinds, SourceSHA: source, HasPR: selection.ForgeHeadSHA != "",
	})
	if err != nil {
		return refusal(err)
	}
	if result.Changed && c.Log != nil {
		run, err := c.DB.GetRun(c.Run.ID)
		if err != nil {
			return db.PRContextBindResult{}, err
		}
		c.Log(fmt.Sprintf("target advanced from %s to %s at %s; rewinding to rebase (target_advance_rewinds=%d)", previous.TargetSHA, candidate.TargetSHA, step, run.TargetAdvanceRewinds))
	}
	return result, nil
}

// checkedRewindTargets runs before BOTH integration loops. Its returned values
// are immutable commit IDs, including for conflict resolution and merge mode.
func checkedRewindTargets(c *pipeline.StepContext, targets []string) (checked []string, cleanup func() error, err error) {
	pending, err := c.DB.GetPendingTargetRewind(c.Run.ID)
	if err != nil {
		return nil, nil, err
	}
	if pending == nil {
		return targets, func() error { return nil }, nil
	}
	previous, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return nil, nil, err
	}
	if previous == nil {
		return nil, nil, fmt.Errorf("pending rewind has no comparison receipt")
	}
	selected, err := ResolvePRTarget(c)
	if err != nil {
		return nil, nil, err
	}
	observed, cleanup, err := observeRewindRemotes(c, selected.TargetBranch, previous.Generation)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()
	head, err := git.HeadSHA(c.Ctx, c.WorkDir)
	if err != nil {
		return nil, cleanup, err
	}
	candidate, err := prComparisonAtTarget(c, selected.TargetBranch, observed.target, head)
	if err != nil {
		return nil, cleanup, err
	}
	if selected.SourceRepo != "" {
		candidate.SourceRepo, candidate.SourceBranch = selected.SourceRepo, selected.SourceBranch
	}
	if selected.PRURL != "" {
		candidate.PRURL, candidate.ForgeHeadSHA = selected.PRURL, selected.ForgeHeadSHA
	}
	// Preserve the provider's canonical spelling already pinned by the guard.
	if previous.SourceBranch == candidate.SourceBranch && scm.SameSourceRepository(resolvedProvider(c), previous.SourceRepo, candidate.SourceRepo) {
		candidate.SourceRepo = previous.SourceRepo
	}
	if head != c.Run.HeadSHA || head != previous.LocalHeadSHA || head != pending.LocalHeadSHA || !samePendingRewindIdentity(pending, previous, candidate, selected) {
		return nil, cleanup, fmt.Errorf("local head or PR identity changed during pending rewind")
	}
	if !sameRewindSource(pending.SourceSHA, observed.source) {
		return nil, cleanup, fmt.Errorf("remote source changed during pending rewind")
	}
	if candidate.TargetSHA != previous.TargetSHA {
		if _, err = classifyTargetAdvance(c, types.StepRebase, selected, previous, candidate, observed.source); err != nil {
			return nil, cleanup, err
		}
		// The old conflict selection and fix budget belong to the discarded
		// comparison. Start with a normal integration of the newly checked SHA.
		c.Fixing, c.AutoFixRound = false, false
		c.PreviousFindings, c.CarriedFindings, c.DeferredFindings = "", "", ""
		c.PRContext, err = c.DB.GetRunPRContext(c.Run.ID)
		if err != nil {
			return nil, cleanup, err
		}

	}
	for _, target := range targets {
		if target != "origin/"+previous.TargetBranch && observed.source != nil {
			checked = append(checked, *observed.source)
		}
	}
	checked = append(checked, observed.target)
	return checked, cleanup, nil
}

func verifyRewindIntegration(c *pipeline.StepContext, target string) error {
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", target, "HEAD"); err != nil {
		return fmt.Errorf("pending rewind did not integrate checked commit %s: %w", target, err)
	}
	return nil
}
