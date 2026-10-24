package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The Document commit cap bounds automatic commits in one run; an explicit
// fix response can authorize one more via documentCapFixGrace. It is not a
// verdict on the documentation, and must never prevent a run from finishing.
//
// Issue #123: the cap was checked BEFORE the agent ran, so the step emitted an
// `ask-user` finding on every round without ever asking whether the
// documentation was still accurate. The finding was an ACTIONABLE one, so
// strict shipping refused to approve the gate ("strict shipping cannot approve
// unresolved actionable findings"), and the gate's other exits were closed
// too: a fix response re-ran the step, which re-emitted the identical finding
// without ever calling the agent. The run was neither passing nor failing,
// and it could never finish.
//
// Two real runs hit it the same day, through two different triggers: an
// automatic rebase renamed the head, and an ordinary reviewed code fix
// changed it. Both reached the cap, because the cap is spent by COMMITS and a
// renamed head spends no more of them.
//
// The fix has two halves:
//
//  1. At the cap, rounds without explicit repair grace run the READ-ONLY gap
//     check. It asks what gaps remain and applies nothing, so it spends no
//     budget. With no gap remaining the step completes and the run continues.
//
//  2. The cap notice itself is INFORMATIONAL (no-op), never an actionable
//     finding. A finding no agent and no human response can resolve must not
//     be able to block an approval forever.

// documentCapFixGrace reports whether this round may land one Document
// commit past the spent cap.
//
// Issue #188: with the cap spent and a REAL gap remaining, every disposition
// of a strict gate was refused. `approve` and `skip` are refused by strict
// shipping, and `fix` re-ran the read-only gap check, which cannot land the
// fix and so re-reported the same gap. The only way out was to abort the run.
//
// A fix response is the one disposition that can resolve a gap, so it is the
// one that earns a commit. The grace is bounded:
//
//   - Only an explicit fix response earns it. The automatic fix loop does
//     not, so the cap still bounds everything the pipeline does on its own.
//     `axi --yes` sends at most one fix per step.
//   - Each response earns at most ONE commit. A proposal over that is
//     discarded like any other over-budget proposal.
//   - The response must select an actionable finding; an informational
//     notice is nothing to fix.
//
// The grace commit is an ordinary repair pass, so it is recorded in the
// run's Document history and counts against the budget like any other.
func documentCapFixGrace(sctx *pipeline.StepContext) bool {
	if !sctx.Fixing || sctx.AutoFixRound || sctx.SkipFixExecution || sctx.FinalizingAnswers {
		return false
	}
	selected, err := types.ParseFindingsJSON(sctx.PreviousFindings)
	if err != nil {
		return false
	}
	return types.HasActionableFindings(selected)
}

// documentCapCheckSection is the read-only gap check's prompt section. The
// agent must not edit anything: the cap forbids another commit, and a check
// that edits would have to be undone.
const documentCapCheckSection = `
Commit budget exhausted (read-only gap check):
- This run has spent its Document commit budget, so you may NOT create any commit and you may NOT edit any file.
- This is a READ-ONLY check. Do not modify, create, delete, stage, or commit anything. Do not run formatters or fixers.
- Report ONLY the documentation gaps that remain unresolved, in the same findings format as the normal pass.
- If the documentation is already accurate for this change, return an empty findings array.
- Report every remaining gap, even one you would have been able to fix in an earlier pass: an unreported gap would let the change ship with stale documentation.`

// documentCapRollbackTimeout bounds the gap check's rollback. The rollback
// has to finish even when the run's own context is already cancelled, so it
// runs on a cleanup context of its own.
var documentCapRollbackTimeout = 60 * time.Second

// documentCapNoticeDescription is informational so the cap alone cannot
// block a run whose documentation is accurate.
const documentCapNoticeDescription = "Document commit limit reached. " + pipeline.DocumentCapWayForward

// runCapGapCheck asks the agent what documentation gaps remain, without
// letting it change anything.
//
// The outcome contract:
//
//   - No gap remains: the step completes. The cap blocked further COMMITS,
//     and the run can finish.
//   - A gap remains: the step parks with that gap as an ordinary finding, so
//     the gap is still visible and still decidable. The cap notice rides
//     along as INFORMATIONAL, never as the actionable finding.
//
// The check's OWN verdict is the authority on the CURRENT gaps. An earlier
// pass's finding is not re-appended: the check reads the current state, so a
// gap it does not re-report is either gone (a later repair removed what
// needed the documentation) or never was. Carrying the old one forward would
// park a run whose documentation is accurate, which is the deadlock this
// whole change exists to remove.
//
// A gap check that edits is not honored: the edits are discarded with the
// same rollback the convergence path uses, so the cap still bounds the
// commits.
func (s *DocumentStep) runCapGapCheck(sctx *pipeline.StepContext, baseSHA, targetBranch string) (*pipeline.StepOutcome, error) {
	return s.runCapGapCheckAttempt(sctx, baseSHA, targetBranch, false)
}

// runCapGapCheckAttempt runs the gap check. reassessing is true when this is
// the SECOND attempt, made because the first one produced a verdict about
// bytes the rollback removed.
//
// A check that edits is bounded to ONE reassessment: a check that edits
// again has not produced a verdict about the current state, so the step
// fails closed instead of looping on an agent that ignores its
// instructions.
func (s *DocumentStep) runCapGapCheckAttempt(sctx *pipeline.StepContext, baseSHA, targetBranch string, reassessing bool) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	headBefore := sctx.Run.HeadSHA
	sctx.Log("Document commit limit reached; running a read-only gap check")

	// The check starts from a clean worktree, exactly like a normal pass, so
	// the rollback below can discard ONLY the check's own proposal.
	dirtyPaths, err := worktreeDirtyPathsOutsideScratch(sctx)
	if err != nil {
		return nil, err
	}
	if len(dirtyPaths) > 0 {
		return documentDirtyEntryOutcome(sctx, dirtyPaths)
	}
	entryPaths, err := documentEntryPaths(sctx)
	if err != nil {
		return nil, err
	}
	retainedScratch, err := documentScratchPaths(sctx, entryPaths)
	if err != nil {
		return nil, err
	}

	prompt := s.buildCapCheckPrompt(sctx, baseSHA, targetBranch)
	// The gap check is a DOCUMENT review, and it says so in its prompt: it
	// asks which documentation gaps remain. It performs no lint duty, so it
	// uses the DOCUMENT schema - the housekeeping schema's category enum
	// would ask the agent to classify a finding the prompt never mentions,
	// and the lint half of the combined pass belongs to the lint step, which
	// falls back to its own pass (the documented fallback).
	//
	// It must not claim the housekeeping purpose either: the presentation
	// surfaces read that purpose as "this invocation did both duties", and
	// this one did not.
	schema := findingsSchema
	purpose := "document-gap-check"

	result, runErr := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: schema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
	})
	// The rollback runs on an UNSUCCESSFUL invocation too. An agent that
	// edited or committed and then failed must not leave that proposal in the
	// worktree: the budget is exhausted, and a surviving commit would also
	// break the head continuity of every later attempt.
	proposed, rollbackErr := s.discardGapCheckProposal(sctx, headBefore, retainedScratch)
	if rollbackErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("agent document gap check: %w (and the gap check's proposal could not be discarded: %v)", runErr, rollbackErr)
		}
		return nil, fmt.Errorf("discard the document gap check's proposal: %w", rollbackErr)
	}
	if runErr != nil {
		return nil, fmt.Errorf("agent document gap check: %w", runErr)
	}
	if result == nil {
		return nil, fmt.Errorf("agent document gap check returned no result")
	}

	var reported Findings
	if err := validateDocumentAnalyzerFindings(result.Output, &reported, false); err != nil {
		return nil, fmt.Errorf("validate document gap check findings: %w", err)
	}
	// Every finding the check reported is a documentation gap. Nothing is
	// filtered out: a finding the check dropped would be a gap nobody sees.
	gaps := reported

	// A gap the agent could not resolve is still a gap: it stays an ordinary
	// finding so the owner can decide. The cap notice never carries the
	// decision, because no fix round can ever clear it.
	gaps = documentCapNotice(gaps)

	// A check that edited produced its verdict against bytes the rollback
	// just removed. Accepting that verdict would complete the step with the
	// ORIGINAL stale documentation still in place - a check that repaired
	// the documentation and then reported no gap would look like a clean
	// pass. The restored bytes have to be reassessed, so the check runs
	// again on them. A check that edits AGAIN has not judged the current
	// state at all, so the step fails closed rather than looping.
	if proposed {
		if reassessing {
			return nil, fmt.Errorf("the document gap check edited the worktree twice; it produced no verdict about the current documentation")
		}
		sctx.Log("the gap check's verdict described discarded changes; reassessing the restored bytes")
		return s.runCapGapCheckAttempt(sctx, baseSHA, targetBranch, true)
	}

	if len(gaps.Items) == 0 {
		sctx.Log("Document commit limit reached; the read-only gap check found no remaining documentation gap")
		return documentCapOutcome(sctx, gaps, "Document commit limit reached; no remaining documentation gap"), nil
	}
	sctx.Log(fmt.Sprintf("Document commit limit reached; the read-only gap check found %d unresolved items", len(gaps.Items)))
	return stoppedDocumentOutcome(sctx, gaps, "Document commit limit reached; stopping further commits"), nil
}

// discardGapCheckProposal rolls back everything the gap check proposed, and
// reports whether it proposed ANYTHING.
//
// A check that edited is not honored: the edits are discarded and the cap
// still holds. The rollback is the same one the convergence path uses, so
// only this check's own proposal is touched.
//
// The boolean return value is the load-bearing one: a verdict produced
// against bytes the rollback just removed describes a state that no longer
// exists, so the caller must not accept it. A check that repaired the
// documentation and then reported no gap would otherwise complete the step
// with the ORIGINAL stale documentation still in place.
func (s *DocumentStep) discardGapCheckProposal(sctx *pipeline.StepContext, headBefore string, retainedScratch []string) (bool, error) {
	// The rollback runs on a CLEANUP context: the run's own context is
	// cancelled when the daemon shuts down, and a cancelled context would
	// fail every Git call of the rollback - leaving the over-budget commit
	// behind and breaking the recorded-head continuity of every later
	// attempt. The rollback must complete even when the run is going away.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), documentCapRollbackTimeout)
	defer cancel()
	cleanup := *sctx
	cleanup.Ctx = cleanupCtx
	// documentCandidatePaths admits the index visibility FIRST, so a check
	// that hid a file behind --assume-unchanged or --skip-worktree cannot
	// make its edit invisible: Git status cannot see such an entry, and the
	// verdict would otherwise be accepted while the committed documentation
	// stayed stale. The check runs on the cleanup context, so it still
	// works when the run was cancelled.
	paths, untracked, err := documentCandidatePaths(&cleanup, headBefore, retainedScratch)
	if err != nil {
		return false, err
	}
	agentHead, err := stepGitRun(&cleanup, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	proposed := len(paths) > 0 || agentHead != headBefore
	if !proposed {
		return false, nil
	}
	sctx.Log("the read-only gap check edited the worktree; discarding its proposal")
	if err := discardDocumentCandidate(&cleanup, headBefore, paths, untracked, retainedScratch); err != nil {
		return true, err
	}
	return true, nil
}

// buildCapCheckPrompt assembles the read-only gap check's prompt: the
// ordinary context and placement policy, plus the read-only instruction that
// replaces the normal pass's "leave edits uncommitted" rule.
func (s *DocumentStep) buildCapCheckPrompt(sctx *pipeline.StepContext, baseSHA, targetBranch string) string {
	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}
	prompt := s.buildPrompt(sctx, baseSHA, targetBranch, ignorePatterns, false)
	// The normal prompt's edit rule and its commit instruction are replaced,
	// not appended: a check that may edit is not a check.
	prompt = strings.Replace(prompt,
		"- Only edit documentation files or doc comments. Do not change executable behavior or tests.",
		"- Do not edit anything. This is a read-only check.", 1)
	prompt = strings.Replace(prompt,
		"- Leave edits uncommitted; the pipeline checks convergence and creates any allowed commit.",
		"- Make no edits at all; the pipeline discards anything you write.", 1)
	// The task steps still read as a repair pass, which contradicts a
	// read-only check. They are replaced, not appended: a check asked to fix
	// things will fix them.
	prompt = strings.Replace(prompt, "2. Find what this change made stale",
		"2. Find what this change made stale (read the diff and the owner documents; do not change them)", 1)
	prompt = strings.Replace(prompt, "3. Fix in the authoritative location",
		"3. Do not fix anything - this pass only decides whether a gap remains", 1)
	prompt = strings.Replace(prompt, "   - Update each altered fact in its owner document. Changed user-facing behavior must leave its authoritative user documentation accurate.\n",
		"   - Note which owner document would need the update; do not write it.\n", 1)
	prompt = strings.Replace(prompt, "   - Remove stale duplicates or reduce them to a short pointer to the owner; do not synchronize full copies.\n",
		"   - Note stale duplicates and the owner each fact belongs to; do not change them.\n", 1)
	prompt = strings.Replace(prompt, "   - Re-read what you changed to verify it now reflects the code.\n",
		"   - Verify the owner's CURRENT content against the code before reporting a gap.\n", 1)
	return prompt + documentCapCheckSection
}

// documentEntryPaths lists the worktree's untracked and staged-addition
// paths, the entry set the scratch classification works on.
func documentEntryPaths(sctx *pipeline.StepContext) ([]string, error) {
	var entryPaths []string
	for _, args := range [][]string{
		{"ls-files", "--others", "--exclude-standard", "-z"},
		{"diff", "--cached", "--diff-filter=A", "--no-renames", "--name-only", "-z", "HEAD"},
	} {
		out, err := stepGitRunRaw(sctx, args...)
		if err != nil {
			return nil, err
		}
		entryPaths = append(entryPaths, documentNULPaths(out)...)
	}
	return entryPaths, nil
}

// documentCapNotice adds the INFORMATIONAL cap notice. It is a no-op
// finding: an actionable finding here is a deadlock, because no fix round
// can clear it and strict shipping refuses to approve it.
//
// The notice is not added when the step found no gap at all: a run whose
// documentation is accurate must not manufacture a finding, and a finding
// nobody has to decide about is noise on a gate the run is about to leave.
func documentCapNotice(findings Findings) Findings {
	for _, finding := range findings.Items {
		if finding.Description == documentCapNoticeDescription {
			return findings
		}
	}
	if len(findings.Items) == 0 {
		return findings
	}
	findings.Items = append(findings.Items, types.Finding{
		Severity: "info", Action: types.ActionNoOp, Description: documentCapNoticeDescription,
	})
	return findings
}

// documentCapOutcome reports a cap stop whose findings carry no actionable
// item. It completes the step: the cap bounded the commits, and there is
// nothing left to decide.
func documentCapOutcome(sctx *pipeline.StepContext, findings Findings, reason string) *pipeline.StepOutcome {
	sctx.Log(reason)
	findings.Summary = reason
	raw, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{NeedsApproval: false, Findings: string(raw), FixSummary: NoChangesAppliedSummary}
}
