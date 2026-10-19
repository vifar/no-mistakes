package steps

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// DocumentStep keeps documentation accurate for the change under its
// placement policy, and - when no deterministic lint command is configured -
// also performs the agent-driven lint duty in the same invocation so the
// pipeline pays one cold agent pass for housekeeping instead of two.
type DocumentStep struct{}

func (s *DocumentStep) Name() types.StepName { return types.StepDocument }

// documentPlacementPolicy is the fail-safe default placement policy. It
// replaces the old exhaustive-synchronization incentive: the agent is
// rewarded for updating each fact's single owner and for consolidation,
// deletion, and pointers - not for synchronizing every prose copy. A trusted
// repository-specific policy (config document.instructions) may narrow or
// clarify these rules but never weaken them.
const documentPlacementPolicy = `Documentation placement policy (fail-safe defaults; repository-specific instructions may narrow or clarify them, never weaken them):
- Every fact or contract has exactly one authoritative owner document. Update the owner; never synchronize prose copies of the same fact.
- When this change leaves an existing duplicate stale, remove the duplicate or reduce it to a short pointer to the owner instead of updating another full copy.
- Do not create a new documentation surface merely to close a perceived gap.
- Do not add incident narratives or postmortems to AGENTS.md or CLAUDE.md. For a durable incident lesson, preserve the operative invariant in its owner document and point to the regression test or authoritative implementation.
- For this step's own documentation work, AGENTS.md and CLAUDE.md are agent memory files loaded into every future agent session, so their content is a human decision, not automated pipeline output. Edit them only to correct or remove information that is factually wrong; never add content because something is missing, never create them when absent, and never restructure or expand them. Formatter and lint passes must not touch them either.
- README.md owns the user-facing product introduction and common usage.
- CONTRIBUTING.md owns contribution mechanics, not product or architecture inventories.
- Code comments own non-obvious local intent, safety invariants, and external constraints - never prose that merely restates code.
- Deep reference docs own detailed conditional material; link to them instead of copying them into always-loaded guidance.
- Generated or schema-backed facts must be generated from their authoritative source and checked for drift, never hand-copied.`

// documentScopeDiscipline bounds the pass to documentation this change made
// stale, replacing the old "be exhaustive across the corpus" instruction.
const documentScopeDiscipline = `Scope discipline:
- Only touch documentation this change made stale, plus direct contradictions that analysis reveals.
- Do not opportunistically rewrite, expand, or restructure unrelated documentation, and do not perform a broad documentation architecture migration here.
- When a larger consolidation is warranted but out of scope, leave this change safe and report one finding proposing the follow-up instead of multiplying edits.
- Preserve load-bearing user guidance, security rationale, compatibility constraints, and onboarding material. A long document is not a defect by itself; duplication and wrong placement are.
- Prefer consolidation, deletion, and pointers to the owner over addition and synchronization.`

// housekeepingLintSection adds the agent-driven lint duty to the combined
// document+lint pass.
const housekeepingLintSection = `

Combined lint duty (same pass - no separate lint agent will run):
- Discover the configured linters and formatters for this repository.
- Run the relevant checks, preferring only the changed files when possible.
- Apply safe formatter, linter, and static-analysis fixes yourself, then re-run the relevant checks.
- Do not run tests or broader behavioral validation.
- Report only unresolved lint, format, or static-analysis issues as findings with "category" set to "lint". Do not report lint issues you already fixed.

Set "category" on every finding: "documentation" for documentation findings, "lint" for lint findings.`

// housekeepingFindingsSchema extends findingsSchema with the per-finding
// category that routes combined-pass findings to their owning gates.
var housekeepingFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]},
					"category": {"type": "string", "enum": ["documentation", "lint"]}
				},
				"required": ["severity", "description", "action", "category"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["findings", "summary"]
}`)

func (s *DocumentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	targetBranch, err := currentPRTargetBranch(sctx)
	if err != nil {
		return nil, err
	}
	baseSHA, err := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, targetBranch)
	if err != nil {
		return nil, err
	}

	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	// Combine the agent-driven lint duty into this pass when no deterministic
	// lint command is configured; the lint step then consumes the result
	// instead of paying its own cold agent invocation.
	combinedLint := sctx.Config.Commands.Lint == ""
	if combinedLint {
		sctx.Shared.ClearHousekeepingLint()
	}
	// Own only this invocation's changes, including commits the agent makes
	// despite the prompt. The clean start makes rejected proposals reversible.
	headBefore := sctx.Run.HeadSHA
	if err := assertDocumentIndexVisible(sctx); err != nil {
		return nil, err
	}
	status, err := stepGitRun(sctx, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	if status != "" {
		return nil, fmt.Errorf("Document requires a clean worktree so convergence can safely discard only its own edits")
	}
	observed, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if observed != headBefore {
		return nil, fmt.Errorf("Document starting head does not match the recorded pipeline head")
	}

	history, err := readDocumentHistory(sctx)
	if err != nil {
		return nil, err
	}
	previous, err := previousDocumentFindings(history)
	if err != nil {
		return nil, err
	}
	if history.last != nil && *history.last.ToHeadSHA == sctx.Run.HeadSHA && len(previous.Items) == 0 {
		return stoppedDocumentOutcome(sctx, previous, "Document already committed and reported no remaining documentation gap"), nil
	}
	limit := sctx.Config.Document.MaxCommitsPerRun

	// Skip entirely when nothing the agent would document has changed. No
	// lint result is stashed, so the lint step falls back to its own pass -
	// neither duty is ever silently skipped.
	changedFiles, err := git.Run(ctx, sctx.WorkDir, "diff", "--name-only", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	if !hasNonIgnoredDocumentChanges(changedFiles, sctx.Config.IgnorePatterns) {
		sctx.Log("no changes to document")
		return &pipeline.StepOutcome{}, nil
	}
	if history.commits >= limit {
		return stoppedDocumentOutcome(sctx, documentCapFinding(previous, false), "Document commit limit reached; stopping further commits"), nil
	}

	if combinedLint {
		sctx.Log("housekeeping: updating documentation and linting in one pass...")
	} else {
		sctx.Log("updating documentation...")
	}

	prompt := s.buildPrompt(sctx, baseSHA, targetBranch, ignorePatterns, combinedLint)
	schema := findingsSchema
	purpose := "document"
	if combinedLint {
		schema = housekeepingFindingsSchema
		purpose = "housekeeping"
	}

	passID, err := sctx.DB.BeginDocumentPass(sctx.Run.ID, headBefore)
	if err != nil {
		return nil, err
	}
	result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: schema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
	})
	if err != nil {
		return nil, fmt.Errorf("agent document: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("agent document returned no result")
	}
	paths, untracked, err := documentCandidatePaths(sctx, headBefore)
	if err != nil {
		return nil, err
	}
	agentHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	agentCommits, err := documentCommitCount(sctx, headBefore, agentHead)
	if err != nil {
		return nil, err
	}
	// Parse before committing only to decide whether a later proposal has
	// converged. The normal validation below still fails opaque output closed.
	var proposed Findings
	proposedErr := validateDocumentAnalyzerFindings(result.Output, &proposed, len(paths) > 0 || agentCommits > 0)
	proposedDocs := proposed
	if combinedLint {
		proposedDocs, _ = splitHousekeepingFindings(proposed)
	}
	repeated, err := documentRepeatsUnchangedInputs(sctx, history.last, headBefore, paths)
	if err != nil {
		return nil, err
	}
	needsCommit, err := stepGitRun(sctx, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	prospectiveCommits := agentCommits
	if needsCommit != "" {
		prospectiveCommits++
	}
	capExceeded := prospectiveCommits > limit-history.commits
	suppressed := capExceeded || repeated
	if suppressed {
		if err := discardDocumentCandidate(sctx, headBefore, paths, untracked); err != nil {
			return nil, err
		}
		if proposedErr != nil {
			return nil, fmt.Errorf("validate document analyzer findings: %w", proposedErr)
		}
		for _, prior := range previous.Items {
			found := false
			for _, current := range proposedDocs.Items {
				priorKey, currentKey := prior, current
				priorKey.ID, currentKey.ID = "", ""
				if reflect.DeepEqual(priorKey, currentKey) {
					found = true
					break
				}
			}
			if !found {
				proposedDocs.Items = append(proposedDocs.Items, prior)
			}
		}
		if capExceeded {
			proposedDocs = documentCapFinding(proposedDocs, true)
		}
		if repeated {
			discarded := types.Finding{
				Severity: "warning", Action: "ask-user", File: paths[0],
				Description: "Document discarded a repeated repair of " + strings.Join(paths, ", ") +
					"; the repair was not applied. Inspect the pending documentation gap before continuing.",
			}
			found := false
			for _, finding := range proposedDocs.Items {
				if finding.Action == discarded.Action && finding.File == discarded.File && finding.Description == discarded.Description {
					found = true
					break
				}
			}
			if !found {
				proposedDocs.Items = append(proposedDocs.Items, discarded)
			}
		}
		raw, _ := json.Marshal(proposedDocs)
		if err := sctx.DB.CompleteDocumentPass(passID, headBefore, 0, string(raw)); err != nil {
			return nil, err
		}
		return stoppedDocumentOutcome(sctx, proposedDocs, "Document converged; discarded the repeated or over-budget proposal without another commit"), nil
	}
	// Attribute agent-created commits only after convergence accepts them.
	if err := recordPipelineHeadRange(sctx, headBefore, agentHead); err != nil {
		return nil, err
	}

	// Commit whatever the agent edited, regardless of how trustworthy its
	// structured output turns out to be.
	commitSummary := extractDocumentSummary(result.Output, "")
	fallbackSummary := "update documentation"
	if combinedLint {
		fallbackSummary = "update documentation and fix lint"
	}
	committed, err := commitAgentFixesWithResult(sctx, s.Name(), commitSummary, fallbackSummary, result)
	if err != nil {
		return nil, err
	}
	changesApplied := committed || sctx.Run.HeadSHA != headBefore

	// Without trustworthy structured output we cannot confirm the agent
	// resolved every gap. Fail the step rather than creating an approval gate:
	// unattended AXI modes can resolve a gate, but must never certify opaque
	// analyzer output.
	var findings Findings
	if result.Output == nil {
		return nil, fmt.Errorf("document analyzer returned no structured findings")
	} else if err := validateDocumentAnalyzerFindings(result.Output, &findings, changesApplied); err != nil {
		return nil, fmt.Errorf("validate document analyzer findings: %w", err)
	}

	docFindings := findings
	if combinedLint {
		var lintFindings Findings
		docFindings, lintFindings = splitHousekeepingFindings(findings)
		lintJSON, err := types.MarshalFindingsJSON(lintFindings)
		if err == nil {
			sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{
				FindingsJSON: lintJSON,
				Summary:      findings.Summary,
			})
			sctx.Log(fmt.Sprintf("housekeeping lint result recorded for the lint step: %d unresolved items", len(lintFindings.Items)))
		}
	}

	needsApproval := len(docFindings.Items) > 0
	findingsJSON, _ := json.Marshal(docFindings)

	sctx.Log(fmt.Sprintf("document findings: %d unresolved items", len(docFindings.Items)))
	commitCount, err := documentCommitCount(sctx, headBefore, sctx.Run.HeadSHA)
	if err != nil {
		return nil, err
	}
	if err := sctx.DB.CompleteDocumentPass(passID, sctx.Run.HeadSHA, commitCount, string(findingsJSON)); err != nil {
		return nil, err
	}
	if err := carryDocumentValidation(sctx, headBefore, sctx.Run.HeadSHA); err != nil {
		return nil, err
	}

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    fixResultSummary(changesApplied),
	}, nil
}

// validateDocumentAnalyzerFindings parses the document pass's structured
// output. A pass that reports no unresolved findings and made no changes -
// nothing for the pipeline to commit and no head movement from the agent's
// own commits - is a legitimate no-op, so its (present) summary may be empty.
// Whenever the agent reports unresolved findings or actually changed files,
// the summary is still required so the outcome names what remains or what
// was done; an absent summary field stays a shape violation in every case.
func validateDocumentAnalyzerFindings(raw []byte, findings *Findings, changesCommitted bool) error {
	if err := unmarshalRequiredFindings(raw, findings, false); err != nil {
		return err
	}
	if strings.TrimSpace(findings.Summary) == "" && (len(findings.Items) > 0 || changesCommitted) {
		return fmt.Errorf("missing summary")
	}
	return nil
}

// buildPrompt assembles the document (or combined document+lint) prompt: the
// placement policy, scope discipline, trusted repository-specific policy,
// the task, and - in combined mode - the lint duty.
func (s *DocumentStep) buildPrompt(sctx *pipeline.StepContext, baseSHA, targetBranch, ignorePatterns string, combinedLint bool) string {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)

	intro := "Keep the project documentation accurate for this change."
	if combinedLint {
		intro = "Perform the combined documentation and lint housekeeping pass for this change."
	}

	editRule := "- Only edit documentation files or doc comments. Do not change executable behavior or tests."
	if combinedLint {
		editRule = "- Documentation edits must only touch documentation files or doc comments. Lint fixes must be safe, mechanical, and behavior-preserving. Never change functional behavior or tests."
	}

	prompt := fmt.Sprintf(
		`%s Analyze what the change made stale, fix each stale fact in its one authoritative location, and report only what you could not resolve.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- base branch: %s
- ignore patterns: %s

%s

%s%s%s

Task:

1. Understand the change
   - Read the diff and changed files to understand what was added, modified, or removed, and the intent of the change.

2. Find what this change made stale
   - For each fact or contract the change altered, locate its one authoritative owner document (README, docs/, doc comments, config examples, etc.).
   - Locate existing duplicates of those facts that are now stale.

3. Fix in the authoritative location
   - Update each altered fact in its owner document. Changed user-facing behavior must leave its authoritative user documentation accurate.
   - Remove stale duplicates or reduce them to a short pointer to the owner; do not synchronize full copies.
   - Re-read what you changed to verify it now reflects the code.

4. Report only what remains
   - Return a finding only for gaps you could not resolve, judgment calls (e.g. ambiguous intent or conflicting docs), or an out-of-scope consolidation worth a follow-up.
   - Do not report gaps you already fixed.
   - If nothing remains, return an empty findings array.%s

Rules:
%s
- Leave edits uncommitted; the pipeline checks convergence and creates any allowed commit.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s`,
		intro,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		targetBranch,
		ignorePatterns,
		documentPlacementPolicy,
		documentScopeDiscipline,
		repositoryDocumentPolicySection(sctx),
		trustedDocumentPolicySection(sctx),
		lintDutySection(combinedLint),
		editRule,
		historySection,
	)
	if sctx.PreviousFindings != "" {
		prompt += `

Previous findings to address:
` + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return prompt
}

// trustedDocumentPolicySection renders the repository-specific documentation
// ownership policy. The value comes from the trusted default-branch copy of
// .no-mistakes.yaml (config.EffectiveRepoConfig), so a contributor's pushed
// branch cannot weaken the rules that gate its own review.
func trustedDocumentPolicySection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Document.Instructions)
	if instructions == "" {
		return ""
	}
	return "\n\nRepository documentation ownership policy (trusted, from the default branch; augments the defaults above and cannot weaken them):\n" +
		sanitizePromptMultilineText(instructions)
}

// repositoryDocumentPolicySection renders the operator's machine-local
// documentation policy for this repository (a repository_overrides entry). It
// is labeled as the operator's so it never reads as the repository's own
// policy, and it only adds to the defaults and the trusted policy.
func repositoryDocumentPolicySection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.Document.RepositoryInstructions)
	if instructions == "" {
		return ""
	}
	return "\n\nMachine-local documentation ownership policy for this repository (from the operator's global no-mistakes config, not from this repository; augments the defaults above and cannot weaken them):\n" +
		sanitizePromptMultilineText(instructions)
}

func lintDutySection(combinedLint bool) string {
	if !combinedLint {
		return ""
	}
	return housekeepingLintSection
}

// splitHousekeepingFindings routes combined-pass findings to their owning
// gates. An uncategorized finding counts as documentation - the stricter
// gate (any documentation finding parks; lint parks only on error/warning) -
// so miscategorization fails safe.
func splitHousekeepingFindings(findings Findings) (doc Findings, lint Findings) {
	doc = Findings{Items: []Finding{}, Summary: findings.Summary}
	lint = Findings{Items: []Finding{}, Summary: findings.Summary}
	for _, item := range findings.Items {
		if item.Category == types.FindingCategoryLint {
			lint.Items = append(lint.Items, item)
			continue
		}
		doc.Items = append(doc.Items, item)
	}
	return doc, lint
}

func hasNonIgnoredDocumentChanges(changedFiles string, ignorePatterns []string) bool {
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range ignorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			return true
		}
	}
	return false
}

func extractDocumentSummary(raw []byte, fallback string) string {
	var payload struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Summary) != "" {
		return payload.Summary
	}
	return fallback
}
