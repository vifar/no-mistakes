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
	latest  *db.DocumentPass
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
		history.latest = p
		if p.CommitCount > 0 {
			history.commits += p.CommitCount
			history.last = p
		}
	}
	return history, nil
}

func previousDocumentFindings(history documentHistory) (Findings, error) {
	var findings Findings
	if history.latest != nil {
		if history.latest.FindingsJSON == nil {
			return findings, fmt.Errorf("completed Document pass has no findings record")
		}
		if err := validateDocumentAnalyzerFindings([]byte(*history.latest.FindingsJSON), &findings, history.latest.CommitCount > 0); err != nil {
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

func documentCapFinding(findings Findings, discarded bool) Findings {
	if !discarded && len(findings.Items) > 0 {
		return findings
	}
	description := "Document commit limit reached; further documentation edits require a new run or an owner decision."
	if discarded {
		description = "Document discarded an over-budget proposal; the repair was not applied. Inspect the pending documentation gaps and commit budget before continuing."
	}
	for _, finding := range findings.Items {
		if finding.Action == "ask-user" && finding.Description == description {
			return findings
		}
	}
	findings.Items = append(findings.Items, types.Finding{
		Severity: "warning", Action: "ask-user", Description: description,
	})
	return findings
}

// documentCandidatePaths includes committed, staged, unstaged, and untracked
// changes. --no-renames preserves both sides of a move and -z preserves names.
//
// Recognized tool scratch is EXCLUDED: a test harness's live database left
// behind by an earlier step is not this pass's proposal, so it must not count
// as a prospective commit and must never be deleted by the rollback.
func documentCandidatePaths(sctx *pipeline.StepContext, from string, retained []string) ([]string, []string, error) {
	// Entry admission precedes the agent. Recheck before trusting status or
	// accepting a pass: flags introduced during it can hide the entire proposal.
	if err := assertDocumentIndexVisible(sctx); err != nil {
		return nil, nil, err
	}
	head, err := stepGitRun(sctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, nil, err
	}
	paths, err := documentRangePaths(sctx, from, head)
	if err != nil {
		return nil, nil, err
	}
	dirty, err := worktreeDirtyPathsOutsideScratch(sctx)
	if err != nil {
		return nil, nil, err
	}
	paths = subtractDocumentPaths(append(paths, dirty...), retained)
	untracked, err := stepGitRunRaw(sctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, fmt.Errorf("read untracked Document edits: %w", err)
	}
	candidates := make(map[string]bool, len(paths))
	for _, path := range paths {
		candidates[path] = true
	}
	var newPaths []string
	for _, path := range documentNULPaths(untracked) {
		if candidates[path] {
			newPaths = append(newPaths, path)
		}
	}
	return paths, newPaths, nil
}

// documentScratchPaths returns the subset of paths that is recognized tool
// scratch. A path is scratch when its first new component marks scratch, or
// when it matches a staging exclude - the SAME identity the automatic
// staging uses, so the two can never disagree about what belongs in the
// branch.
func documentScratchPaths(sctx *pipeline.StepContext, paths []string) ([]string, error) {
	headDirs := map[string]bool{}
	var scratch []string
	for _, path := range paths {
		component, err := firstNewScratchComponent(sctx, headDirs, path, false)
		if err != nil {
			return nil, err
		}
		if component != "" {
			scratch = append(scratch, path)
			continue
		}
		if _, excluded := matchesStagingExclude(sctx, path); excluded {
			scratch = append(scratch, path)
		}
	}
	return scratch, nil
}

// subtractDocumentPaths removes every path in drop from paths, preserving the
// order of the rest.
func subtractDocumentPaths(paths, drop []string) []string {
	if len(drop) == 0 {
		return paths
	}
	excluded := make(map[string]bool, len(drop))
	for _, path := range drop {
		excluded[path] = true
	}
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if !excluded[path] {
			kept = append(kept, path)
		}
	}
	return kept
}

func documentRangePaths(sctx *pipeline.StepContext, from, to string) ([]string, error) {
	if _, err := documentCommitCount(sctx, from, to); err != nil {
		return nil, err
	}
	commits, err := stepGitRun(sctx, "rev-list", from+".."+to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, commit := range strings.Fields(commits) {
		out, err := stepGitRunRaw(sctx, "diff-tree", "-r", "-m", "--no-commit-id", "--name-only", "--no-renames", "--ignore-submodules=none", "-z", commit)
		if err != nil {
			return nil, fmt.Errorf("read Document commit paths: %w", err)
		}
		paths = append(paths, documentNULPaths(out)...)
	}
	return paths, nil
}

func documentNULPaths(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
}

// Porcelain cannot prove clean entry while either index flag hides raw bytes.
// Refuse even an apparently unmodified entry; never clear a user's flags.
func assertDocumentIndexVisible(sctx *pipeline.StepContext) error {
	out, err := stepGitRunRaw(sctx, "ls-files", "-v", "-z")
	if err != nil {
		return fmt.Errorf("inspect Document index flags: %w", err)
	}
	var hidden []string
	count := 0
	for _, entry := range documentNULPaths(out) {
		if len(entry) < 3 || entry[1] != ' ' {
			return fmt.Errorf("Document cannot verify index flags: malformed index entry")
		}
		// -v lowercases the tag for assume-unchanged, including 's' when
		// both flags are set. 'S' independently identifies skip-worktree.
		if entry[0] == 'S' || (entry[0] >= 'a' && entry[0] <= 'z') {
			count++
			if len(hidden) < 10 {
				hidden = append(hidden, strconv.Quote(entry[2:]))
			}
		}
	}
	if count > 0 {
		return fmt.Errorf("worktree has %d entries hidden from status by index flags; Document cannot verify a clean entry (first %d paths: %s)", count, len(hidden), strings.Join(hidden, ", "))
	}
	return nil
}

// A filename alone cannot prove repetition: intervening input changes may
// require a different repair to that same file. Bind suppression to the exact
// accepted input tree, using the entry head captured before the agent runs.
func documentRepeatsUnchangedInputs(sctx *pipeline.StepContext, previous *db.DocumentPass, entryHead string, paths []string) (bool, error) {
	// A matching committed tree says nothing about bytes hidden by index flags.
	if err := assertDocumentIndexVisible(sctx); err != nil {
		return false, err
	}
	if previous == nil || len(paths) == 0 {
		return false, nil
	}
	previousTree, err := stepGitRun(sctx, "rev-parse", "--verify", *previous.ToHeadSHA+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("read previous Document input tree: %w", err)
	}
	entryTree, err := stepGitRun(sctx, "rev-parse", "--verify", entryHead+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("read current Document input tree: %w", err)
	}
	if previousTree == "" || entryTree == "" {
		return false, fmt.Errorf("Document input tree identity is empty")
	}
	if previousTree != entryTree {
		return false, nil
	}
	previousPaths, err := documentRangePaths(sctx, previous.FromHeadSHA, *previous.ToHeadSHA)
	if err != nil {
		return false, fmt.Errorf("read previous Document paths: %w", err)
	}
	allowed := make(map[string]bool)
	for _, p := range previousPaths {
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

// The caller ruled out real edits and recorded inherited scratch BEFORE
// invoking the agent. Only that invocation's proposal may be discarded,
// including its own commits; retained scratch is excluded even if staged.
// A failed restoration aborts the step instead of leaving edits for Push.
func discardDocumentCandidate(sctx *pipeline.StepContext, from string, paths, untracked, retained []string) error {
	// Recheck before ANY mutation: the agent may have set flags after entry.
	// Refusal leaves its commits, files, flags and pending pass untouched.
	if err := assertDocumentIndexVisible(sctx); err != nil {
		return err
	}
	head, err := stepGitRun(sctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	// Restore only proposal paths present in the entry tree or current index.
	// Paths added and removed inside an intermediate commit need no restore;
	// untracked-only paths are removed separately below.
	tracked := make(map[string]bool)
	for _, args := range [][]string{
		{"ls-tree", "-r", "--name-only", "-z", from},
		{"ls-files", "--cached", "-z"},
	} {
		out, err := stepGitRunRaw(sctx, args...)
		if err != nil {
			return fmt.Errorf("read converged Document restoration paths: %w", err)
		}
		for _, path := range documentNULPaths(out) {
			tracked[path] = true
		}
	}
	var restore []string
	for _, path := range paths {
		if tracked[path] {
			restore = append(restore, path)
			delete(tracked, path)
		}
	}
	if len(restore) > 0 {
		args := append([]string{"--literal-pathspecs", "restore", "--no-recurse-submodules", "--source=" + from, "--staged", "--worktree", "--"}, restore...)
		if _, err := stepGitRun(sctx, args...); err != nil {
			return fmt.Errorf("restore converged Document proposal paths: %w", err)
		}
	}
	if len(untracked) > 0 {
		args := append([]string{"--literal-pathspecs", "clean", "-f", "--"}, untracked...)
		if _, err := stepGitRun(sctx, args...); err != nil {
			return fmt.Errorf("remove converged Document additions: %w", err)
		}
	}
	// Rewind the ref without rewriting unrelated index or worktree paths.
	// The expected old head refuses an intervening ref move.
	if head != from {
		if _, err := stepGitRun(sctx, "update-ref", "-m", "no-mistakes: discard Document proposal", "HEAD", from, head); err != nil {
			return fmt.Errorf("restore converged Document head: %w", err)
		}
	}
	dirty, err := worktreeDirtyPathsOutsideScratch(sctx)
	if err != nil {
		return err
	}
	if len(subtractDocumentPaths(dirty, retained)) > 0 {
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
