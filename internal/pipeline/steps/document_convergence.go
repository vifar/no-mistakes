package steps

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type documentHistory struct {
	commits int
	last    *db.DocumentPass
}

func readDocumentHistory(sctx *pipeline.StepContext) (documentHistory, error) {
	var history documentHistory
	passes, err := sctx.DB.GetDocumentPasses(sctx.Run.ID)
	if err != nil {
		return history, err
	}
	for i := range passes {
		p := &passes[i]
		if p.ToHeadSHA == nil {
			return history, fmt.Errorf("Document pass %s was interrupted; refusing another pass with an uncertain commit budget", p.ID)
		}
		if p.CommitCount > 0 {
			history.commits += p.CommitCount
			history.last = p
		}
	}
	return history, nil
}

func previousDocumentFindings(history documentHistory) (Findings, error) {
	var findings Findings
	if history.last != nil {
		if history.last.FindingsJSON == nil {
			return findings, fmt.Errorf("committed Document pass has no findings record")
		}
		if err := validateDocumentAnalyzerFindings([]byte(*history.last.FindingsJSON), &findings, true); err != nil {
			return findings, fmt.Errorf("read previous Document findings: %w", err)
		}
	}
	return findings, nil
}

func stoppedDocumentOutcome(sctx *pipeline.StepContext, findings Findings, reason string) *pipeline.StepOutcome {
	sctx.Log(reason)
	findings.Summary = reason
	raw, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: len(findings.Items) > 0, Findings: string(raw), FixSummary: NoChangesAppliedSummary}
}

func documentCapFinding(findings Findings) Findings {
	if len(findings.Items) == 0 {
		findings.Items = append(findings.Items, types.Finding{
			Severity: "warning", Action: "ask-user",
			Description: "Document commit limit reached; further documentation edits require a new run or an owner decision.",
		})
	}
	return findings
}

// documentCandidatePaths includes committed, staged, unstaged, and untracked
// changes. --no-renames preserves both sides of a move and -z preserves names.
func documentCandidatePaths(sctx *pipeline.StepContext, from string) ([]string, []string, error) {
	tracked, err := stepGitRun(sctx, "diff", "--name-only", "--no-renames", "-z", from)
	if err != nil {
		return nil, nil, fmt.Errorf("read Document edits: %w", err)
	}
	untracked, err := stepGitRun(sctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, fmt.Errorf("read untracked Document edits: %w", err)
	}
	newPaths := documentNULPaths(untracked)
	return append(documentNULPaths(tracked), newPaths...), newPaths, nil
}

func documentNULPaths(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
}

func documentReeditsPreviousFiles(sctx *pipeline.StepContext, previous *db.DocumentPass, paths []string) (bool, error) {
	if previous == nil || len(paths) == 0 {
		return false, nil
	}
	out, err := stepGitRun(sctx, "diff", "--name-only", "--no-renames", "-z", *previous.ToHeadSHA+"^", *previous.ToHeadSHA)
	if err != nil {
		return false, fmt.Errorf("read previous Document paths: %w", err)
	}
	allowed := make(map[string]bool)
	for _, p := range documentNULPaths(out) {
		allowed[p] = true
	}
	for _, p := range paths {
		if !allowed[p] {
			return false, nil
		}
	}
	return true, nil
}

func documentCommitCount(sctx *pipeline.StepContext, from, to string) (int, error) {
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", from, to); err != nil {
		return 0, fmt.Errorf("Document rewrote its starting head: %w", err)
	}
	out, err := stepGitRun(sctx, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid Document commit count %q", out)
	}
	return count, nil
}

// The caller proved a clean starting tree BEFORE invoking the agent. Only
// that invocation's edits may be discarded, including its own commits. A
// failed restoration aborts the step instead of leaving edits for Push.
func discardDocumentCandidate(sctx *pipeline.StepContext, from string, untracked []string) error {
	if _, err := stepGitRun(sctx, "reset", "--hard", from); err != nil {
		return fmt.Errorf("restore converged Document head: %w", err)
	}
	if len(untracked) > 0 {
		args := append([]string{"--literal-pathspecs", "clean", "-f", "--"}, untracked...)
		if _, err := stepGitRun(sctx, args...); err != nil {
			return fmt.Errorf("remove converged Document additions: %w", err)
		}
	}
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("suppressed Document edits remain in the worktree")
	}
	return nil
}

func carryDocumentValidation(sctx *pipeline.StepContext, from, to string) error {
	if from == to || sctx.Run.Shipping != nil {
		return nil
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return err
	}
	if run == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != from {
		return nil
	}
	docOnly, err := docOnlyRange(sctx, from, to)
	if err != nil {
		return err
	}
	if !docOnly {
		return nil
	}
	carried, err := sctx.DB.CarryValidationOverDocOnlyCommits(sctx.Run.ID, from, to)
	if err != nil {
		return err
	}
	if carried {
		sctx.Run.ReviewApprovedHeadSHA = &to
		sctx.Log(fmt.Sprintf("Document commit is documentation-only; recorded carry of Review and Test credit from %s to %s", shortObjectID(from), shortObjectID(to)))
	}
	return nil
}
