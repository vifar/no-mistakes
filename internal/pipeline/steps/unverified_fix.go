package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// UnverifiedFixError indicates that a fix commit could not be verified by the pipeline
// (e.g. verification was blocked, missing required tooling, or unverified).
// The phase must stop at the gate and commit nothing rather than advancing as a normal head.
type UnverifiedFixError struct {
	Step    types.StepName
	Commit  string
	Summary string
	Reason  string
}

func (e *UnverifiedFixError) Error() string {
	if e.Commit != "" {
		return fmt.Sprintf("%s: unverified fix commit %s %q: %s", e.Step, e.Commit, e.Summary, e.Reason)
	}
	if e.Summary != "" {
		return fmt.Sprintf("%s: fix verification blocked: %q: %s", e.Step, e.Summary, e.Reason)
	}
	return fmt.Sprintf("%s: fix verification blocked: %s", e.Step, e.Reason)
}

// CheckUnverifiedFixSummary reports whether a fixer summary indicates that verification
// was blocked, skipped, or unverified.
func CheckUnverifiedFixSummary(summary string) string {
	lower := strings.ToLower(summary)
	for _, marker := range []string{
		"tests blocked",
		"test blocked",
		"verification blocked",
		"regression tests unverified",
		"tests unverified",
		"test unverified",
		"verification unverified",
		"blocked without",
		"blocked by missing",
		"blocked by",
		"unverified",
	} {
		if strings.Contains(lower, marker) {
			return marker
		}
	}
	return ""
}

// CheckUnverifiedFixCommitMessage checks whether a commit subject represents an unverified
// fix commit (such as a pipeline fix commit or commit with explicit blocked/unverified testing).
func CheckUnverifiedFixCommitMessage(subject string) string {
	lower := strings.ToLower(subject)
	isPipelineFix := strings.Contains(lower, "no-mistakes(")

	// Process-of-verification blocked indicators (e.g. tests blocked, verification blocked)
	processMarkers := []string{
		"tests blocked",
		"test blocked",
		"verification blocked",
		"regression tests unverified",
		"tests unverified",
		"test unverified",
		"verification unverified",
		"blocked without",
		"blocked by missing",
	}
	for _, m := range processMarkers {
		if strings.Contains(lower, m) {
			return m
		}
	}

	// For pipeline fix commits (no-mistakes(...)), also check broader markers
	if isPipelineFix {
		for _, m := range []string{
			"unverified",
			"blocked by",
		} {
			if strings.Contains(lower, m) {
				return m
			}
		}
	}
	return ""
}

// checkReviewHeadVerified inspects the commits under review between baseSHA and HEAD
// (as well as any bound uncertified range) to ensure no unverified review fix commits are reviewed
// as normal heads.
func checkReviewHeadVerified(ctx context.Context, sctx *pipeline.StepContext, baseSHA string) error {
	if sctx == nil || sctx.WorkDir == "" || sctx.Run == nil {
		return nil
	}
	headSHA := strings.TrimSpace(sctx.Run.HeadSHA)
	if headSHA == "" {
		var err error
		headSHA, err = git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil
		}
	}
	baseSHA = strings.TrimSpace(baseSHA)

	if baseSHA != "" && headSHA != "" && baseSHA != headSHA {
		out, err := git.Run(ctx, sctx.WorkDir, "log", "--format=%H %s", baseSHA+".."+headSHA)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, " ", 2)
				if len(parts) == 2 {
					if marker := CheckUnverifiedFixCommitMessage(parts[1]); marker != "" {
						return &UnverifiedFixError{
							Step:    types.StepReview,
							Commit:  parts[0],
							Summary: parts[1],
							Reason:  fmt.Sprintf("unverified review fix commit cannot be reviewed as a normal head (%s)", marker),
						}
					}
				}
			}
		}
	}

	// Check any bound uncertified pipeline range
	if sctx.UncertifiedFromSHA != "" && sctx.UncertifiedToSHA != "" {
		out, err := git.Run(ctx, sctx.WorkDir, "log", "--format=%H %s", sctx.UncertifiedFromSHA+".."+sctx.UncertifiedToSHA)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, " ", 2)
				if len(parts) == 2 {
					if marker := CheckUnverifiedFixCommitMessage(parts[1]); marker != "" {
						return &UnverifiedFixError{
							Step:    types.StepReview,
							Commit:  parts[0],
							Summary: parts[1],
							Reason:  fmt.Sprintf("uncertified fixer commit was unverified (%s)", marker),
						}
					}
				}
			}
		}
	}

	return nil
}

// resetWorktreeToHead discards any working tree modifications and untracked files
// so that a stopped/blocked fix round leaves no unverified changes in the worktree.
func resetWorktreeToHead(sctx *pipeline.StepContext) error {
	if sctx == nil || sctx.WorkDir == "" {
		return nil
	}
	ctx := sctx.Ctx
	if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	_, _ = git.Run(ctx, sctx.WorkDir, "clean", "-fd")
	return nil
}
