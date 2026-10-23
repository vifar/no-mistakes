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

// errTargetComparisonAdvanced marks the one comparison failure that a
// pre-publication boundary can still recover from: the LIVE target, merge
// base, or diff no longer matches the run's pinned receipt because the target
// branch moved. It is NOT a security failure and NOT a proof of tampering -
// the caller re-reads the target and classifies the advance with the same
// rules every other target-advance boundary uses.
//
// Every other comparison failure (a changed PR identity, a changed head, an
// unreadable receipt) keeps failing closed.
var errTargetComparisonAdvanced = errors.New("live target comparison advanced")

func sameRewindSource(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func sameRewindIdentity(p *db.PRContext, c db.PRContextCandidate) bool {
	return p.PRURL == c.PRURL && p.SourceRepo == c.SourceRepo && p.SourceBranch == c.SourceBranch && p.TargetBranch == c.TargetBranch && p.ForgeHeadSHA == c.ForgeHeadSHA
}

// samePendingRewindIdentity validates the full discovery observation, even
// when a discovered PR cannot attach because its head differs from local HEAD.
func samePendingRewindIdentity(c *pipeline.StepContext, pending *db.TargetAdvanceRewind, previous *db.PRContext, candidate db.PRContextCandidate, selected pipeline.PRTargetSelection) bool {
	if previous == nil || !sameRewindIdentity(previous, candidate) {
		return false
	}
	if pending.ForgeHeadSHA == nil {
		observation, err := c.DB.GetTargetRewindPRObservation(c.Run.ID)
		if err != nil {
			return false
		}
		if observation != nil {
			return previous.PRURL == "" && selected.PRURL == "" && selected.ObservedPRURL == observation.PRURL && selected.ForgeHeadSHA == observation.HeadSHA
		}
		return previous.PRURL == "" && selected.PRURL == "" && selected.ForgeHeadSHA == "" && selected.ObservedPRURL == ""
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
	out, err := git.RunNetwork(ctx, c.WorkDir, "ls-remote", "--exit-code", "--refs", remote, "refs/heads/"+branch)
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

func attributeTargetAdvanceHead(c *pipeline.StepContext, previous *db.PRContext, candidate db.PRContextCandidate) (*db.PRContext, error) {
	if err := c.DB.ValidatePipelineHeadForward(c.Run.ID, previous.LocalHeadSHA, candidate.LocalHeadSHA); err != nil {
		return nil, fmt.Errorf("target advance with unexplained local head: %w", err)
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", previous.LocalHeadSHA, candidate.LocalHeadSHA); err != nil {
		return nil, fmt.Errorf("target advance with non-descendant pipeline head: %w", err)
	}
	own, err := prComparisonAtTarget(c, previous.TargetBranch, previous.TargetSHA, candidate.LocalHeadSHA)
	if err != nil {
		return nil, err
	}
	own.PRURL, own.SourceRepo, own.SourceBranch, own.ForgeHeadSHA = previous.PRURL, previous.SourceRepo, previous.SourceBranch, previous.ForgeHeadSHA
	if _, err := c.DB.AdvanceRunPRContext(c.Run.ID, own); err != nil {
		return nil, err
	}
	return c.DB.GetRunPRContext(c.Run.ID)
}

// attributeRebasedHead attributes a head move to the run's own completed
// Rebase step.
//
// A rebase REWRITES the head: the same logical changes land on a new base, so
// the new head is not a descendant of the one it replaces and the recorded
// commit edges of a fix round do not exist. The proof is therefore the run's
// own durable record rather than the commit graph:
//
//   - the Rebase step completed (the caller proves it), and
//   - the head it produced is the run's recorded head, and
//   - the PR identity, the target and the source are unchanged, so only the
//     head the run itself rewrote is accepted.
//
// The receipt then follows the rebased head, so the run's own rebase is not
// refused as an unexplained comparison change - which would otherwise strand
// every run on a branch that is behind its target.
func attributeRebasedHead(c *pipeline.StepContext, previous *db.PRContext, candidate db.PRContextCandidate) (*db.PRContext, error) {
	if c == nil || c.Run == nil {
		return nil, fmt.Errorf("rebased head attribution requires a run")
	}
	if candidate.LocalHeadSHA == "" || candidate.LocalHeadSHA != c.Run.HeadSHA {
		return nil, fmt.Errorf("rebased head is not the run's own head")
	}
	// The comparison is recomputed against the rebased head, so the receipt's
	// merge base and diff digest describe the head the run actually carries.
	own, err := prComparisonAtTarget(c, previous.TargetBranch, previous.TargetSHA, candidate.LocalHeadSHA)
	if err != nil {
		return nil, err
	}
	own.PRURL, own.SourceRepo, own.SourceBranch, own.ForgeHeadSHA = previous.PRURL, previous.SourceRepo, previous.SourceBranch, previous.ForgeHeadSHA
	if _, err := c.DB.AdvanceRunPRContext(c.Run.ID, own); err != nil {
		return nil, err
	}
	return c.DB.GetRunPRContext(c.Run.ID)
}

func classifyTargetAdvance(c *pipeline.StepContext, step types.StepName, selection pipeline.PRTargetSelection, previous *db.PRContext, candidate db.PRContextCandidate, source *string) (db.PRContextBindResult, error) {
	refusal := func(cause error) (db.PRContextBindResult, error) {
		return db.PRContextBindResult{}, fmt.Errorf("PR comparison changed after rebase; start a new run for the current target and head: %w", cause)
	}
	if previous == nil || !sameRewindIdentity(previous, candidate) || candidate.LocalHeadSHA != c.Run.HeadSHA || candidate.LocalHeadSHA != previous.LocalHeadSHA || previous.TargetSHA == candidate.TargetSHA {
		return refusal(fmt.Errorf("%w: PR identity or local head differs from the recorded comparison", db.ErrTargetAdvanceIneligible))
	}
	if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", previous.TargetSHA, candidate.TargetSHA); err != nil {
		return refusal(fmt.Errorf("target is not a strict fast-forward: %w", err))
	}
	maxRewinds := config.DefaultMaxTargetAdvanceRewinds
	if c.Config != nil {
		maxRewinds = c.Config.Rebase.MaxTargetAdvanceRewinds
	}
	result, err := c.DB.RewindRunPRContext(c.Run.ID, candidate, db.TargetAdvanceRewindRequest{
		ExpectedGeneration: previous.Generation, Step: step, MaxRewinds: maxRewinds, SourceSHA: source, HasPR: selection.ForgeHeadSHA != "", ObservedPRURL: selection.ObservedPRURL, ObservedForgeHeadSHA: selection.ForgeHeadSHA,
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

// detectTargetAdvanceAtPublication re-reads the live target comparison at a
// pre-publication boundary and classifies a target advance that the run can
// still rewind. It returns true when the caller must restart from Rebase.
//
// The publication boundary (Push) is still PRE-publication: nothing is
// published until the push itself settles, so the same rewind the Review..Lint
// boundaries use is available here. A run that already published is not
// eligible, and neither is a strict shipping run.
//
// The comparison is read from the LIVE target, never from a cached ref, so a
// target that moved after the last step boundary is seen exactly as the
// attestation write would see it.
//
// An unreadable target is NOT a detected advance. The attestation write
// re-reads the same target and fails closed on its own comparison, so this
// check never becomes a new failure mode: it only ever recognizes an advance
// it can prove.
func detectTargetAdvanceAtPublication(c *pipeline.StepContext, step types.StepName) (bool, error) {
	if c == nil || c.DB == nil || c.Run == nil || c.Run.Shipping != nil {
		return false, nil
	}
	previous, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return false, err
	}
	if previous == nil {
		return false, nil
	}
	eligible, err := c.DB.CanRewindTarget(c.Run.ID, step)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}
	selection := pipeline.PRTargetSelection{TargetBranch: previous.TargetBranch}
	if c.PRTarget != nil && c.PRTarget.TargetBranch == previous.TargetBranch {
		selection = *c.PRTarget
	}
	selection.TargetBranch = previous.TargetBranch
	// The run's own recorded PR identity is the selection's identity. A live
	// selection that names a different PR is not this run's PR.
	if previous.PRURL != "" {
		if selection.PRURL != "" && selection.PRURL != previous.PRURL {
			return false, nil
		}
		selection.PRURL, selection.ForgeHeadSHA = previous.PRURL, previous.ForgeHeadSHA
		selection.SourceRepo, selection.SourceBranch = previous.SourceRepo, previous.SourceBranch
	}
	candidate, err := readPRComparison(c, selection)
	if err != nil {
		if c.Log != nil {
			c.Log(fmt.Sprintf("skipping the target-advance check at %s: %v", step, err))
		}
		return false, nil
	}
	// The comparison keeps the run's own recorded identity. A live selection
	// that names a different PR is not this run's PR, and a comparison
	// without one is a comparison the run has not attached yet.
	candidate.PRURL, candidate.ForgeHeadSHA = previous.PRURL, previous.ForgeHeadSHA
	candidate.SourceRepo, candidate.SourceBranch = previous.SourceRepo, previous.SourceBranch
	if candidate.LocalHeadSHA != c.Run.HeadSHA {
		return false, nil
	}
	if candidate.TargetSHA == previous.TargetSHA && candidate.TargetBranch == previous.TargetBranch {
		return false, nil
	}
	// The source branch and the attached PR must still be the ones this run
	// recorded. A changed PR identity is not a target advance.
	if !sameRewindIdentity(previous, candidate) {
		return false, nil
	}
	observed, cleanup, observeErr := observeRewindRemotes(c, candidate.TargetBranch, previous.Generation)
	if observeErr != nil {
		if c.Log != nil {
			c.Log(fmt.Sprintf("skipping the target-advance check at %s: %v", step, observeErr))
		}
		return false, nil
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	exact, err := prComparisonAtTarget(c, candidate.TargetBranch, observed.target, candidate.LocalHeadSHA)
	if err != nil {
		return false, err
	}
	candidate.TargetSHA, candidate.MergeBaseSHA, candidate.DiffDigest = exact.TargetSHA, exact.MergeBaseSHA, exact.DiffDigest
	if candidate.TargetSHA == previous.TargetSHA {
		return false, nil
	}
	if candidate.LocalHeadSHA != previous.LocalHeadSHA {
		previous, err = attributeTargetAdvanceHead(c, previous, candidate)
		if err != nil {
			return false, err
		}
	}
	if _, err := classifyTargetAdvance(c, step, selection, previous, candidate, observed.source); err != nil {
		return false, err
	}
	return true, nil
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
	if head != c.Run.HeadSHA || head != previous.LocalHeadSHA || head != pending.LocalHeadSHA || !samePendingRewindIdentity(c, pending, previous, candidate, selected) {
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
	// Do not replay source history whose content already survives an earlier
	// rebase. A source not yet contained still needs normal integration; the
	// force-push target list continues to exclude deliberately discarded work.
	for _, target := range targets {
		if target != "origin/"+previous.TargetBranch && observed.source != nil {
			contained := false
			if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", *observed.source, head); err == nil {
				contained = true
			} else if tree, err := git.Run(c.Ctx, c.WorkDir, "merge-tree", "--write-tree", head, *observed.source); err == nil {
				headTree, err := git.Run(c.Ctx, c.WorkDir, "rev-parse", head+"^{tree}")
				if err != nil {
					return nil, cleanup, err
				}
				contained = tree == headTree
			}
			if !contained {
				checked = append(checked, *observed.source)
			}
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
