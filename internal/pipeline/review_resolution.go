package pipeline

import (
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// resolveReviewRoundFindings is shared by the effective gate and the archived
// rewind boundary. A round's raw output is not a verdict on its carried set.
func resolveReviewRoundFindings(sctx *StepContext, outstanding string, pending, selected []string, outcome *StepOutcome, current string) (effective, remaining string, answered, scoped []types.WithdrawnFinding) {
	verification := dropReviewQuestionFindingsJSON(current)
	outstanding = dropReviewQuestionFindingsJSON(outstanding)
	if sctx.FinalizingAnswers {
		var retractable []types.WithdrawnFinding
		for _, w := range outcome.WithdrawnFindings {
			if !slices.Contains(selected, w.ID) {
				retractable = append(retractable, w)
			}
		}
		outstanding, answered = dropWithdrawnFindingsJSON(outstanding, retractable)
	}
	outstanding, scoped = resolveReviewCarry(sctx, outstanding, pending, outcome, verification)
	remaining = mergeOutstandingFindingsJSON(outstanding, current, outcome.ReviewedPaths)
	withdrawn := append(append([]types.WithdrawnFinding(nil), answered...), scoped...)
	return recordWithdrawnFindingsJSON(remaining, withdrawn), remaining, answered, scoped
}

func resolvedReviewBoundary(sctx *StepContext, outcome *StepOutcome, outstanding string, pending, selected []string) *StepOutcome {
	boundary := *outcome
	current := normalizeFindingsJSON(outcome.Findings, string(types.StepReview))
	boundary.Findings, _, _, _ = resolveReviewRoundFindings(sctx, outstanding, pending, selected, outcome, current)
	boundary.ReviewCarryResolved = true
	findings, err := types.ParseFindingsJSON(boundary.Findings)
	if err != nil || len(findings.Items) != 0 {
		boundary.NeedsApproval = true
		boundary.ReviewApprovedHeadSHA = ""
	}
	return &boundary
}

func resolveReviewCarry(sctx *StepContext, raw string, pending []string, outcome *StepOutcome, current string) (string, []types.WithdrawnFinding) {
	if raw == "" {
		return raw, nil
	}
	unchanged := make(map[string]bool)
	var applied []types.WithdrawnFinding
	if strictReviewResolutionEvidence(sctx, outcome, current) {
		comparison, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
		if err == nil && comparison != nil {
			findings, err := types.ParseFindingsJSON(raw)
			if err == nil {
				for _, item := range findings.Items {
					file := normalizeCoveredPath(item.File)
					if file == "" || file != item.File || strings.HasPrefix(file, "/") || file == ".." || strings.HasPrefix(file, "../") {
						continue
					}
					// Both the diff base and the pinned target must agree with the
					// reviewed head for this literal path, including mode/deletion.
					headEntry, headErr := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "ls-tree", "-z", comparison.HeadSHA, "--", file)
					baseEntry, baseErr := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "ls-tree", "-z", comparison.MergeBaseSHA, "--", file)
					targetEntry, targetErr := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "ls-tree", "-z", comparison.TargetSHA, "--", file)
					if headErr == nil && baseErr == nil && targetErr == nil && string(headEntry) == string(baseEntry) && string(headEntry) == string(targetEntry) && selectedAnchorWasChanged(sctx, item, comparison.TargetSHA) {
						unchanged[item.ID] = true
					}
				}
			}
		}
		eligible := acceptedScopeFindings(sctx, raw)
		for _, id := range pending {
			delete(eligible, id)
		}
		var withdrawals []types.WithdrawnFinding
		for _, w := range outcome.WithdrawnFindings {
			if eligible[w.ID] && strings.TrimSpace(w.Reason) != "" {
				withdrawals = append(withdrawals, w)
			}
		}
		raw, applied = dropWithdrawnFindingsJSON(raw, withdrawals)
	}
	return resolveVerifiedFindingsJSON(raw, pending, outcome.ReviewedPaths, outcome.ReviewablePaths, current, unchanged), applied
}

func scopeFindingsForReview(sctx *StepContext, raw string) string {
	var ids []string
	for id := range acceptedScopeFindings(sctx, raw) {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ""
	}
	return filterFindingsJSON(raw, ids)
}

func selectedAnchorWasChanged(sctx *StepContext, item types.Finding, target string) bool {
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil {
		return false
	}
	for _, round := range rounds {
		if err := decodeRoundDecision(round); err != nil {
			return false
		}
	}
	var heads []string
	for _, round := range rounds {
		identity, selected := selectedFindingIdentities([]*db.StepRound{round})[item.ID]
		if !selected || findingKey(identity) != findingKey(item) {
			continue
		}
		if round.ReviewedHeadSHA == nil || len(*round.ReviewedHeadSHA) != 40 {
			return false
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "cat-file", "-e", *round.ReviewedHeadSHA+"^{commit}"); err != nil {
			return false
		}
		heads = append(heads, *round.ReviewedHeadSHA)
	}
	for _, head := range heads {
		diff, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", target, head, "--", item.File)
		if err == nil && string(diff) == item.File+"\x00" && !reviewAnchorRenamed(sctx, head, item.File) {
			return true
		}
	}
	return false
}

func reviewAnchorRenamed(sctx *StepContext, oldHead, file string) bool {
	entry, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "--literal-pathspecs", "ls-tree", "-z", sctx.Run.HeadSHA, "--", file)
	if err != nil {
		return true
	}
	changes, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", "-z", "--find-renames", "--find-copies", "--no-ext-diff", "--no-textconv", oldHead, sctx.Run.HeadSHA)
	if err != nil {
		return true
	}
	fields := strings.Split(strings.TrimSuffix(string(changes), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return false
	}
	for i := 0; i < len(fields); {
		if fields[i] == "" || i+1 >= len(fields) {
			return true
		}
		// Git can encode a rewritten rename as D+A. A missing literal
		// anchor plus any addition is ambiguous, rather than proof of removal.
		if fields[i] == "A" && len(entry) == 0 {
			return true
		}
		rename := fields[i][0] == 'R' || fields[i][0] == 'C'
		if rename {
			if i+2 >= len(fields) || fields[i+1] == file {
				return true
			}
			i += 3
		} else {
			i += 2
		}
	}
	return false
}

// A scope disposition is the existing human selection's complement, bound by
// content identity. A selection to fix always wins over a later omission.
func acceptedScopeFindings(sctx *StepContext, raw string) map[string]bool {
	if sctx.DB == nil || sctx.StepResultID == "" {
		return nil
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil {
		return nil
	}
	for _, round := range rounds {
		if err := decodeRoundDecision(round); err != nil {
			return nil
		}
		if humanDecidedRound(round) && (round.SelectedFindingIDs == nil || strings.TrimSpace(*round.SelectedFindingIDs) == "null") {
			return nil
		}
	}
	decided := earlierUserDecisionIDs(rounds, raw)
	for id := range earlierChosenToFixIDs(rounds, raw) {
		delete(decided, id)
	}
	return decided
}

func strictReviewResolutionEvidence(sctx *StepContext, outcome *StepOutcome, current string) bool {
	if sctx.Run == nil || sctx.Run.Shipping == nil || sctx.DB == nil || outcome.NeedsApproval || outcome.ReviewComparisonGeneration < 1 || outcome.ReviewApprovedHeadSHA == "" || len(outcome.ReviewablePaths) == 0 {
		return false
	}
	findings, err := types.ParseFindingsJSON(current)
	if err != nil || len(findings.Items) != 0 {
		return false
	}
	coverage := make(map[string]bool)
	for _, file := range outcome.ReviewablePaths {
		if file == "" || normalizeCoveredPath(file) != file {
			return false
		}
		coverage[file] = false
	}
	for _, file := range outcome.ReviewedPaths {
		if _, exists := coverage[file]; !exists {
			return false
		}
		coverage[file] = true
	}
	for _, covered := range coverage {
		if !covered {
			return false
		}
	}
	c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
	if err != nil || c == nil || c.Generation != outcome.ReviewComparisonGeneration || c.HeadSHA != outcome.ReviewApprovedHeadSHA || c.HeadSHA != sctx.Run.HeadSHA || c.TargetBranch != sctx.Run.Shipping.TargetBranch {
		return false
	}
	if sctx.Run.Shipping.Mode == types.ShippingImmutableSource && c.TargetSHA != sctx.Run.Shipping.TargetSHA {
		return false
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil || head != c.HeadSHA {
		return false
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil || status != "" {
		return false
	}
	base, digest, err := shipping.Compare(sctx.Ctx, sctx.WorkDir, c.TargetSHA, c.HeadSHA)
	return err == nil && base == c.MergeBaseSHA && digest == c.DiffDigest
}
