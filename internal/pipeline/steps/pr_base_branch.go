package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// ValidateRunPRBaseBranchName checks a per-run PR base branch name using the
// same Git ref rules as pr.base_branch in repo config.
func ValidateRunPRBaseBranchName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", nil
	}
	if _, err := evidence.NormalizeBranch(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

// VerifyRemoteBranchExists reports whether origin has refs/heads/<branch>.
func VerifyRemoteBranchExists(ctx context.Context, workDir, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil
	}
	sha, err := git.LsRemote(ctx, workDir, "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("look up remote branch %q: %w", branch, err)
	}
	if sha == "" {
		return fmt.Errorf("remote branch %q does not exist on origin", branch)
	}
	return nil
}

// runPRBaseBranch returns the per-run PR base branch override stored on the
// run record, or "" when unset.
func runPRBaseBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.PRBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.PRBaseBranch)
}

// verifyStrictPRBaseBranch binds strict shipping to the target persisted on the
// run. A provider's cached PR list result is not sufficient because the target
// can change after the PR was discovered.
func verifyStrictPRBaseBranch(ctx context.Context, sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	if sctx == nil || sctx.Run == nil || sctx.Run.Shipping == nil {
		return nil
	}
	if pr == nil {
		return fmt.Errorf("strict shipping cannot verify a missing PR")
	}
	expected := strings.TrimSpace(sctx.Run.Shipping.TargetBranch)
	if expected == "" {
		return fmt.Errorf("strict shipping run has no pinned PR target")
	}
	reader, ok := host.(scm.PRBaseBranchReader)
	if !ok {
		return fmt.Errorf("strict shipping requires a provider that can read the live PR target")
	}
	actual, err := pipeline.ReadPRContext(ctx, sctx.Log, func() (string, error) { return reader.GetPRBaseBranch(ctx, pr) })
	if err != nil {
		return fmt.Errorf("read strict shipping PR target: %w", err)
	}
	actual = strings.TrimSpace(actual)
	if actual == "" {
		return fmt.Errorf("strict shipping could not verify the live PR target")
	}
	if pr != nil {
		pr.BaseBranch = actual
	}
	if actual != expected {
		return fmt.Errorf("strict shipping target mismatch: PR targets %q, pinned target is %q", actual, expected)
	}
	return nil
}
