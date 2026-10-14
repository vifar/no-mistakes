package plugin

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"strings"
)

// ReadPRFacts uses one repository-scoped view. Protocol v1 does not support
// cross-repository fork PRs: both source and target belong to the repository
// supplied with every invocation. A full live head is required for comparison
// proof; old plugins omitting it fail closed without inventing evidence.
func (h *Host) ReadPRFacts(ctx context.Context, pr *scm.PR) (scm.PRFacts, error) {
	result, live, err := h.viewPR(ctx, pr)
	if err != nil {
		return scm.PRFacts{}, err
	}
	if result.PR == nil || strings.TrimSpace(result.PR.HeadBranch) == "" || live.BaseBranch == "" || !fullFactsSHA(live.HeadSHA) {
		return scm.PRFacts{}, h.protocolf(CmdPRView, "comparison facts require head_branch, base_branch, and a full head_sha")
	}
	var state scm.PRState
	switch strings.ToLower(strings.TrimSpace(result.State)) {
	case "open":
		state = scm.PRStateOpen
	case "merged":
		state = scm.PRStateMerged
	case "closed":
		state = scm.PRStateClosed
	default:
		return scm.PRFacts{}, h.protocolf(CmdPRView, "unknown PR state %q", result.State)
	}
	facts := scm.PRFacts{PR: *live, State: state, SourceRepository: h.opts.Repository.Path, SourceBranch: result.PR.HeadBranch, HeadSHA: live.HeadSHA, BaseBranch: live.BaseBranch}
	if state == scm.PRStateMerged {
		if !h.Capabilities().MergedProof {
			return scm.PRFacts{}, h.protocolf(CmdPRView, "merged comparison requires merged_proof")
		}
		proof, err := h.GetMergedProof(ctx, live, live.HeadSHA)
		if err != nil {
			return scm.PRFacts{}, err
		}
		if !proof.Merged || !fullFactsSHA(proof.MergeCommitSHA) {
			return scm.PRFacts{}, h.protocolf(CmdPRMerged, "merged comparison lacks a full merge commit")
		}
		facts.MergeCommitSHA = proof.MergeCommitSHA
	}
	return facts, nil
}

// FindOpenPRFacts preserves protocol v1's unique branch lookup. Plugins must
// refuse ambiguous branch matches rather than choose one candidate. The view
// read back must still identify the same open repository-scoped source.
func (h *Host) FindOpenPRFacts(ctx context.Context, repository, branch string) ([]scm.PRFacts, error) {
	if repository != h.opts.Repository.Path || strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("provider plugin PR source differs from invocation repository")
	}
	pr, err := h.FindPR(ctx, branch, "")
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}
	facts, err := h.ReadPRFacts(ctx, pr)
	if err != nil {
		return nil, err
	}
	if facts.State != scm.PRStateOpen || facts.SourceBranch != branch {
		return nil, h.protocolf(CmdPRFind, "discovered PR is not the requested open source branch")
	}
	return []scm.PRFacts{facts}, nil
}

func fullFactsSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
