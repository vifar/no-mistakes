package steps

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	defaultBaseBranchTipResolveWindow = 30 * time.Second
	defaultPublishedHeadResolveWindow = 30 * time.Second
)

func ciPollInterval(sctx *pipeline.StepContext, elapsed, override time.Duration) time.Duration {
	interval := override
	if interval == 0 {
		interval = pollInterval(elapsed)
	}
	if sctx.Run.Shipping != nil && interval < 5*time.Minute {
		return 5 * time.Minute
	}
	return interval
}

// CI monitoring status messages. These are surfaced to the user and parsed by
// the TUI and the agent-facing axi commands to distinguish passed checks from
// checks that are still running. The canonical strings live in cimonitor so all
// producers and consumers agree on them.
const (
	ciChecksPassedMsg           = cimonitor.ChecksPassedMsg
	ciNoChecksPassedMsg         = cimonitor.NoChecksPassedMsg
	ciChecksRunningMsg          = cimonitor.ChecksRunningMsg
	ciChecksAwaitingApprovalMsg = cimonitor.ChecksAwaitingApprovalMsg
	ciBehindBaseMsg             = cimonitor.BehindBaseMsg
)

// CIStep monitors an open PR until it is merged, closed, or its configured idle
// timeout elapses, and repairs CI failures through the executor's shared
// findings machinery.
//
// Execute has the same shape as the review step's: when the executor
// re-executes it as a fix round (sctx.Fixing), it first repairs the findings
// selected for that round (sctx.PreviousFindings), then it monitors. Each
// settled observation - no in-scope check still executing, every authorized
// transient rerun spent, at least one issue present - returns one finding per issue
// carrying its action (ciObservationFindings), and the executor decides what
// happens next: auto-fix findings enter the auto_fix.ci loop as fix rounds,
// ask-user findings park for a decision. The step never counts or limits fix
// rounds itself; the executor does, durably, from the round history.
//
// Readiness uses scm.CheckScope so an entirely excluded observed rollup is
// distinguished from an unobserved empty one. Excluded contexts must not
// re-enter findings or repairs through retained selections or rerun state.
type CIStep struct {
	lastFixedChecks      string                    // encoded targets of the last published repair, so a poll that still shows them is not re-escalated
	lastFixedCompletedAt map[string]checkFreshness // terminally failed check freshness at the observation the last repair targeted
	observedCompletedAt  map[string]checkFreshness // terminally failed check freshness at the observation whose findings a fix round may repair
	pendingFixSummary    string                    // one-line summary of the repair this execution published, attached to the outcome it ends with
	pendingRepairPublish bool
	transientReruns      checkRerunBudget // per-check rerun budget spent on provider-reported transient failures
	pollIntervalOverride time.Duration    // if set, overrides computed poll interval (for testing)
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
	// baseBranchTip resolves the current tip SHA of the upstream default
	// branch. The bool is false when the SHA is a fallback/unknown value and
	// must not re-arm the timeout. Overridable for testing; defaults to
	// fetching the upstream default branch.
	baseBranchTip func(context.Context) (string, bool)
	// replayBase is the resolved base tip an in-flight rebase repair targets,
	// set only for the duration of that repair's recording. recordRepair
	// proves a pure replay against exactly this commit and nothing else.
	replayBase string
	// recordCheckScope persists the scope this execution evaluates under.
	// Overridable for testing; defaults to recordCIChecksScope.
	recordCheckScope func(*pipeline.StepContext, *scm.CheckScope) error
}

// SetPollIntervalOverride is a test hook; production leaves the override unset.
func (s *CIStep) SetPollIntervalOverride(d time.Duration) *CIStep {
	s.pollIntervalOverride = d
	return s
}

// SetWaitForNextPoll is a test hook; production sleeps for the poll interval.
func (s *CIStep) SetWaitForNextPoll(fn func(context.Context, time.Duration) error) *CIStep {
	s.waitForNextPoll = fn
	return s
}

// SetNow is a test hook; production uses time.Now.
func (s *CIStep) SetNow(fn func() time.Time) *CIStep {
	s.now = fn
	return s
}

// SetBaseBranchTip is a test hook; production fetches the upstream default branch.
func (s *CIStep) SetBaseBranchTip(fn func(context.Context) (string, bool)) *CIStep {
	s.baseBranchTip = fn
	return s
}

// TransientRerunRecorded reports whether a rerun budget entry exists for name.
// Tests use this to assert monitor accounting without reading unexported state.
func (s *CIStep) TransientRerunRecorded(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.transientReruns.rollup[name]
	return ok
}

func (s *CIStep) Name() types.StepName { return types.StepCI }

// ReconcileApprovalGate re-checks the PR after the CI step has parked at an
// approval gate. A PR can be merged or closed after a timeout/failure gate was
// recorded; either terminal state supersedes the stale gate just as it does in
// the normal CI polling loop. Open, unknown, and provider-error states remain
// parked so reconciliation never guesses success.
func (s *CIStep) ReconcileApprovalGate(sctx *pipeline.StepContext) (bool, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return false, fmt.Errorf("%w: %w", pipeline.ErrFatalGateReconciliation, err)
	}
	if err := sctx.Ctx.Err(); err != nil {
		return false, err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		return false, fmt.Errorf("cannot check PR state: %s", skipReason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return false, err
	}

	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return false, fmt.Errorf("run has no PR URL")
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return false, fmt.Errorf("extract PR number: %w", err)
	}
	state, err := host.GetPRState(sctx.Ctx, &scm.PR{Number: prNumber, URL: prURL})
	if err != nil {
		return false, err
	}
	switch state {
	case scm.PRStateMerged:
		if err := verifyMergedProof(sctx.Ctx, host, &scm.PR{Number: prNumber, URL: prURL}, sctx.Run.HeadSHA); err != nil {
			return false, err
		}
		if err := verifyShippingMergedProof(sctx, host, &scm.PR{Number: prNumber, URL: prURL}); err != nil {
			return false, err
		}
		if err := observeCIState(sctx, "merged"); err != nil {
			return false, err
		}
		if sctx.Log != nil {
			sctx.Log("PR has been merged; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateClosed:
		if err := observeCIState(sctx, "closed"); err != nil {
			return false, err
		}
		if sctx.Log != nil {
			sctx.Log("PR has been closed; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateOpen:
		if err := observeCIState(sctx, "open"); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("PR state is unresolved: %q", state)
	}
}

// VerifyApprovalOverride implements pipeline.ApprovalOverrideVerifier. It
// re-polls the live checks once, synchronously, at the moment a human answers
// ActionApprove on a CI gate raised by ciFailureOutcome, so an approval over
// checks that do not satisfy the shared scope can never be silently reported
// as a plain "outcome=passed" - see the interface doc for the incident this exists
// for. Resolution uses scm.ScopedChecks.Ready, as the polling loop does,
// rather than !hasFailingChecks: pending, cancelled, missing, and otherwise
// unresolved in-scope checks cannot prove a clean pass. Read-only: unlike
// Execute and ReconcileApprovalGate, it never fixes, reruns, or pushes
// anything, and it never blocks the approval itself - it
// only decides how the resulting completion is recorded.
func (s *CIStep) VerifyApprovalOverride(sctx *pipeline.StepContext) (string, error) {
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return "", err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		return fmt.Sprintf("could not verify live CI state: %s", skipReason), nil
	}
	if err := host.Available(ctx); err != nil {
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}

	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return "could not verify live CI state: run has no PR URL", nil
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	// The delivered commit is named on every read, not only when review-bot
	// identity needs the head's rollup: a host that binds its reads to the
	// named commit refuses one that is not evidence for it (GitLab's head
	// pipeline can still point at an older commit), and an operator override
	// must never be recorded from another commit's checks. Review-bot identity
	// comes from that same head read, exactly as the polling loop names it.
	pr.HeadSHA = sctx.Run.HeadSHA
	checks, err := host.GetChecks(ctx, pr)
	if err == nil {
		err = scm.ValidateCheckHeads(checks, pr.HeadSHA)
	}
	if err != nil {
		if errors.Is(err, scm.ErrHeadChanged) {
			// The provider answered; nothing it offered is evidence for the
			// commit this run delivered. That is not a broken tool and not a
			// clean pass: record it as an override naming the mismatch, the
			// same refusal the polling loop waits on.
			return fmt.Sprintf("live checks for %s are not for the commit being delivered: %v", prURL, err), nil
		}
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}
	targetBranch, err := currentPRTargetBranch(sctx)
	if err != nil {
		return fmt.Sprintf("could not verify live CI target: %v", err), nil
	}
	pr.BaseBranch = targetBranch
	// A parked gate is verified under the scope it was raised with, not the
	// configuration loaded now: a later edit to external_checks must not
	// reclassify the failure the operator is approving over. A run that
	// recorded nothing predates check scoping (for example a gate parked by an
	// older build); like the strict receipt, it keeps the legacy behavior of
	// judging every check rather than the live configuration, which may be
	// less strict than the gate's original evaluation.
	policy, recorded, err := recordedCIChecksScope(sctx)
	if err != nil {
		return fmt.Sprintf("could not verify live CI scope: %v", err), nil
	}
	if !recorded {
		policy = scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll}
	}
	overrideScope := scm.NewCheckScope(host, policy, targetBranch, func(msg string) {
		if sctx.Log != nil {
			sctx.Log(msg)
		}
	})
	scoped := overrideScope.Apply(ctx, pr, checks)
	excludedChecks := scoped.ReviewBotPresenceMarkers(overrideScope.ExternalReviewBotChecks())
	checks = scoped.Relevant
	if scoped.Ready() {
		if err := verifyCIMonitorComparison(sctx, host, checks); err != nil {
			return fmt.Sprintf("could not verify live CI comparison: %v", err), nil
		}
		// A green head can still carry a green review bot's unresolved
		// comments under ci.review_bot_comments: always; approving over
		// them is an override, not a clean pass.
		botFindings, err := s.greenReviewBotFindings(sctx, host, pr, policy, checks, excludedChecks...)
		if err != nil {
			return fmt.Sprintf("could not verify review bot comments: %v", err), nil
		}
		if len(botFindings.Items) > 0 {
			return fmt.Sprintf("live checks for %s passed but %s", prURL, botFindings.Summary), nil
		}
		return "", nil
	}
	if len(checks) == 0 {
		return fmt.Sprintf("live checks for %s: no checks reported", prURL), nil
	}
	return fmt.Sprintf("live checks for %s not all passed: %s", prURL, strings.Join(unresolvedCheckNames(checks), ", ")), nil
}

func verifyMergedProof(ctx context.Context, host scm.Host, pr *scm.PR, expectedHead string) error {
	if !host.Capabilities().MergedProof {
		return nil
	}
	proofHost, ok := host.(scm.MergedProofHost)
	if !ok {
		return fmt.Errorf("SCM provider advertises merged proof but does not implement it")
	}
	proof, err := proofHost.GetMergedProof(ctx, pr, expectedHead)
	if err != nil {
		return fmt.Errorf("verify merged PR proof: %w", err)
	}
	if !proof.Merged {
		return fmt.Errorf("verify merged PR proof: PR %s is not merged", pr.Number)
	}
	if proof.Number != pr.Number || proof.URL != pr.URL {
		return fmt.Errorf("verify merged PR proof: proof identifies PR %s at %q, want PR %s at %q", proof.Number, proof.URL, pr.Number, pr.URL)
	}
	if expectedHead != "" && proof.HeadSHA != expectedHead {
		return fmt.Errorf("verify merged PR proof: %w: expected %s, got %s", scm.ErrHeadChanged, expectedHead, proof.HeadSHA)
	}
	return nil
}

// observeCIState records PR truth without completing CI or the run. The same
// observation is available to the executor's post-CI comparison guard.
func observeCIState(sctx *pipeline.StepContext, state string) error {
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, state); err != nil {
		return err
	}
	// Keep the in-memory observation monotonic like its durable counterpart.
	if sctx.Run.PRState != nil && (*sctx.Run.PRState == "merged" ||
		(*sctx.Run.PRState == "closed" && state != "merged")) {
		return nil
	}
	sctx.Run.PRState = &state
	return nil
}

func terminalCIOutcome(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, state scm.PRState) (*pipeline.StepOutcome, bool, error) {
	if sctx.Run.Shipping != nil && (state == scm.PRStateMerged || state == scm.PRStateClosed) {
		if state == scm.PRStateMerged {
			if err := verifyMergedProof(sctx.Ctx, host, pr, sctx.Run.HeadSHA); err != nil {
				return nil, true, err
			}
			if err := verifyShippingMergedProof(sctx, host, pr); err != nil {
				return nil, true, err
			}
		}
		if err := observeCIState(sctx, strings.ToLower(string(state))); err != nil {
			return nil, true, err
		}
		if state == scm.PRStateMerged {
			return &pipeline.StepOutcome{MergedHeadSHA: sctx.Run.HeadSHA}, true, nil
		}
		return &pipeline.StepOutcome{}, true, nil
	}

	switch state {
	case scm.PRStateMerged:
		if err := verifyMergedProof(sctx.Ctx, host, pr, sctx.Run.HeadSHA); err != nil {
			return nil, true, err
		}
		if err := observeCIState(sctx, "merged"); err != nil {
			return nil, true, err
		}
		sctx.Log("PR has been merged!")
		return &pipeline.StepOutcome{}, true, nil
	case scm.PRStateClosed:
		if err := observeCIState(sctx, "closed"); err != nil {
			return nil, true, err
		}
		sctx.Log("PR has been closed")
		return &pipeline.StepOutcome{}, true, nil
	default:
		return nil, false, nil
	}
}

func (s *CIStep) Execute(sctx *pipeline.StepContext) (outcome *pipeline.StepOutcome, err error) {
	if sctx.Fixing && !sctx.SkipFixExecution {
		if err := sctx.DB.InvalidateStatusValidation(sctx.Run.ID); err != nil {
			return nil, err
		}
	}
	refusalFindings := ""
	if sctx.StepResultID != "" {
		stepResult, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil {
			return nil, fmt.Errorf("restore CI protected-path refusal: %w", err)
		}
		if stepResult != nil && stepResult.FindingsJSON != nil {
			refusalFindings = *stepResult.FindingsJSON
		}
	}
	retryRefusal := sctx.Fixing && pipeline.HasProtectedPathRefusal(refusalFindings)
	// A fix round repairs the findings the executor selected for it, unless
	// this re-entry is the retry of a retained repair a protected-path refusal
	// interrupted: that repair is finished first and nothing new is requested.
	repairRequested := sctx.Fixing && !retryRefusal
	if repairRequested && sctx.Run.Shipping != nil && sctx.Run.Shipping.Mode == types.ShippingImmutableSource {
		return nil, fmt.Errorf("immutable source forbids CI repairs")
	}
	defer func() {
		if outcome != nil {
			if s.pendingFixSummary != "" {
				outcome.FixSummary = s.pendingFixSummary
			}
			outcome.RepairPublished = s.pendingRepairPublish
		}
		s.pendingFixSummary = ""
		s.pendingRepairPublish = false
	}()
	defer func() {
		if !retryRefusal {
			return
		}
		if refusal := pipeline.ProtectedPathOutcome(err); refusal != nil {
			outcome, err = refusal, nil
			return
		}
		findings, _ := types.ParseFindingsJSON(refusalFindings)
		findings.Summary = "Retained CI repair could not finish; resolve the failure and retry with fix"
		if err != nil {
			findings.Summary += ": " + safeurl.RedactText(err.Error())
		} else if outcome != nil && outcome.SkipReason != "" {
			findings.Summary += ": " + safeurl.RedactText(outcome.SkipReason)
		}
		encoded, _ := types.MarshalFindingsJSON(findings)
		outcome, err = &pipeline.StepOutcome{NeedsApproval: true, Findings: encoded}, nil
	}()
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	// A run recovered after a restart resumes the rerun budget it already
	// spent. Without this the fresh in-memory budget would grant reruns the
	// documented limit already accounted for.
	s.loadRerunBudget(sctx)
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping CI: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: skipReason}, nil
	}
	if err := host.Available(ctx); err != nil {
		if pluginContractBroken(err) {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("skipping CI: %v", err))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: err.Error()}, nil
	}

	// Get PR URL from run record
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		// Try to refresh from DB in case PR step set it
		run, _ := sctx.DB.GetRun(sctx.Run.ID)
		if run != nil && run.PRURL != nil {
			prURL = *run.PRURL
			sctx.Run.PRURL = run.PRURL
		}
	}
	if prURL == "" {
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{Skipped: true, SkipReason: "no PR URL found"}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	baseBranch := effectivePRBaseBranch(sctx)
	if sctx.Run.Shipping != nil {
		if err := verifyStrictPRBaseBranch(ctx, sctx, host, pr); err != nil {
			clearCIMonitorReady(sctx)
			return nil, err
		}
	} else {
		// A resumed ordinary run may have a different trusted configuration
		// than the run that created this PR. Follow its live target for conflict
		// repair and tip monitoring, as before.
		if reader, ok := host.(scm.PRBaseBranchReader); ok {
			if actual, readErr := reader.GetPRBaseBranch(ctx, pr); readErr == nil {
				pr.BaseBranch = actual
			} else if pluginContractBroken(readErr) {
				return nil, readErr
			}
		}
		if strings.TrimSpace(pr.BaseBranch) != "" {
			baseBranch = strings.TrimSpace(pr.BaseBranch)
		}
	}
	defer func() {
		if err != nil || outcome == nil || outcome.Skipped || outcome.RestartFrom != "" {
			return
		}
		results, resolveErr := resolveCIReviewSupport(sctx, host, pr)
		if resolveErr != nil {
			if errors.Is(resolveErr, errReviewSupportHeadAdvanced) {
				outcome, err = &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
				return
			}
			outcome, err = nil, fmt.Errorf("resolve Review CI support: %w", resolveErr)
			return
		}
		if appendErr := appendOwnerSupportResults(outcome, results); appendErr != nil {
			outcome, err = nil, fmt.Errorf("record Review CI support: %w", appendErr)
		}
	}()
	if retryRefusal {
		if err := setCIMonitorReadiness(sctx, false, false); err != nil {
			return nil, err
		}
		repair, err := s.retryProtectedPathRepair(sctx)
		if err != nil {
			return nil, err
		}
		retryRefusal = false
		if repair.Revalidate {
			return &pipeline.StepOutcome{RestartFrom: repair.restartFrom()}, nil
		}
	}
	// A PR can close while CI is waiting. Observe that lifecycle before asking
	// for an open PR's target branch, which is no longer available after close.
	if state, stateErr := host.GetPRState(ctx, pr); stateErr == nil {
		if terminal, done, terminalErr := terminalCIOutcome(sctx, host, pr, state); done || terminalErr != nil {
			return terminal, terminalErr
		}
	}
	if sctx.Run.Shipping == nil {
		baseBranch, err = currentPRTargetBranch(sctx)
		if err != nil {
			return nil, err
		}
	}
	pr.BaseBranch = baseBranch
	// Reuse the scope until the live target changes. Retarget handling below
	// replaces it before classifying checks against the new target.
	checkScope := newCIChecksScope(sctx, host, baseBranch)
	// The recorded scope is what approval verification and the strict receipt
	// read later. An execution that cannot record the policy it runs under
	// must not raise a gate under it: the stale record would judge that gate.
	record := s.recordCheckScope
	if record == nil {
		record = recordCIChecksScope
	}
	if err := record(sctx, checkScope); err != nil {
		clearCIMonitorReady(sctx)
		return nil, err
	}
	// Re-record once the scope resolves its required set, before anything
	// classified under it can raise a gate.
	resolutionRecorded := false
	recordResolution := func() error {
		if resolutionRecorded {
			return nil
		}
		if _, ok := checkScope.Resolution(); !ok {
			return nil
		}
		if err := record(sctx, checkScope); err != nil {
			clearCIMonitorReady(sctx)
			return err
		}
		resolutionRecorded = true
		return nil
	}
	if repairRequested {
		pr.HeadSHA = sctx.Run.HeadSHA
		checks, err := host.GetChecks(ctx, pr)
		if err != nil {
			return nil, fmt.Errorf("scope retained CI findings: %w", err)
		}
		if headErr := scm.ValidateCheckHeads(checks, pr.HeadSHA); headErr != nil {
			// A carried replay awaiting its Test restart is not published
			// yet, so no live check belongs to the run's head: retained work
			// is scoped by the identity it names against the target's
			// requirements alone. Any other mismatch still fails closed.
			carryPending, pendingErr := sctx.DB.RunTestRestartPending(sctx.Run.ID)
			if pendingErr != nil || !carryPending {
				return nil, headErr
			}
			checks = nil
		}
		if err := scopeRetainedCIFindings(sctx, checkScope, pr, checks); err != nil {
			return nil, err
		}
		if err := recordResolution(); err != nil {
			return nil, err
		}
		repairOutcome, err := s.repairFromFindings(sctx, host, pr)
		if err != nil {
			return nil, err
		}
		if repairOutcome != nil {
			return repairOutcome, nil
		}
		// A repair that recorded nothing (the fixer failed) must not fall
		// through to monitoring the old published head while a carried review
		// approval still awaits its restart at Test: green checks there would
		// complete the run with the carried head never published. A commit the
		// failed fixer still left on top of the carried head goes through
		// recordRepair like any other, so it revokes the approval and restarts
		// at Review; only the unchanged carried head restarts at Test.
		pending, err := sctx.DB.RunTestRestartPending(sctx.Run.ID)
		if err != nil {
			return nil, err
		}
		if pending {
			head, err := stepGitHeadSHA(sctx)
			if err != nil {
				return nil, fmt.Errorf("resolve head after failed CI fix: %w", err)
			}
			repair, err := s.recordRepair(sctx, head)
			if err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{RestartFrom: repair.restartFrom(), Findings: sctx.DeferredFindings}, nil
		}
	}

	// CITimeout semantics: <0 (or "unlimited" in config) means never
	// self-terminate; 0 means the value was never configured, so fall back
	// to the default; >0 is an explicit finite idle timeout.
	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}

	if unlimited {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (no timeout, until merged or closed)...", prNumber))
	} else {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (timeout: %s)...", prNumber, timeout))
	}
	// State the repair policy once, at entry, rather than at every poll: which
	// of the two very differently priced paths a repair will take is the single
	// most useful thing to know when reading a CI step log after the fact, and
	// it cannot be inferred from the repair line alone until a repair happens.
	sctx.Log(fmt.Sprintf("CI repair policy: %s (ci.revalidate_repairs: %t)", ciRepairPolicyDescription(sctx), ciRevalidatesRepairs(sctx)))
	now := s.now
	if now == nil {
		now = time.Now
	}
	baseBranchTip := s.baseBranchTip
	if baseBranchTip == nil {
		baseBranchTip = func(ctx context.Context) (string, bool) {
			return resolveRunDefaultBranchTip(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
		}
	}
	started := now()
	// timeoutAnchor is the point the idle timeout is measured from. It re-arms
	// to now() whenever the base branch advances, while started stays fixed so
	// poll-interval and grace-period pacing are unaffected by re-arming.
	timeoutAnchor := started
	lastBaseTip := ""
	// behindBaseTip is the base tip a green, BEHIND PR was last seen at. The
	// rebase repair fires only when the next poll still sees it green and
	// BEHIND at that same tip, so a base that keeps moving does not spend a
	// rebase and a full revalidation on a head that is stale again at once.
	behindBaseTip := ""
	mergeabilityBlockedReason := ""
	var timeoutFailingChecks []scm.CheckTarget
	timeoutMergeConflict := false
	lastMonitorLog := ""
	lastScopeLog := ""
	consecutiveCheckErrs := 0
	consecutiveBotCommentErrs := 0
	timeoutOutcome := func() (*pipeline.StepOutcome, error) {
		sctx.Log("CI timeout reached")
		var outcome *pipeline.StepOutcome
		if len(timeoutFailingChecks) > 0 || timeoutMergeConflict {
			outcome = ciFailureOutcome(timeoutFailingChecks, timeoutMergeConflict, "CI timed out with known failures still present")
		} else if mergeabilityBlockedReason != "" {
			outcome = ciMergeabilityOutcome("mergeability check timed out", mergeabilityBlockedReason)
		} else {
			outcome = ciMonitoringTimeoutOutcome()
		}
		return ciTerminalRepairOutcome(outcome, Findings{}, sctx.DeferredFindings), nil
	}
	waitForPoll := func() error {
		interval := s.pollIntervalOverride
		interval = ciPollInterval(sctx, now().Sub(started), interval)
		if !unlimited {
			remaining := timeout - now().Sub(timeoutAnchor)
			if remaining > 0 && remaining < interval {
				interval = remaining
			}
		}
		// The wait between polls does no work, and a PR can be monitored for
		// days: give the run's concurrency slot back for it. The poll itself is
		// a few forge API calls and goes unslotted; any agent turn the monitor
		// starts (a CI fix, a conflict repair) re-acquires one in RunAgent.
		sctx.YieldSlot()
		waitForNextPoll := s.waitForNextPoll
		if waitForNextPoll == nil {
			waitForNextPoll = func(ctx context.Context, interval time.Duration) error {
				select {
				case <-time.After(interval):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return waitForNextPoll(ctx, interval)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}
		if sctx.Run.Shipping != nil {
			if err := verifyStrictPRBaseBranch(ctx, sctx, host, pr); err != nil {
				clearCIMonitorReady(sctx)
				return nil, err
			}
		}
		botCommentReadFailed := false
		behindObserved := false

		// The target a PR points at decides which checks are required of it,
		// and it can change while this step monitors. Re-read it before any
		// check is classified, and rebuild the scope for the new target so a
		// requirement of the new base is never treated as informational and a
		// failure only the old base required is never dispatched. Strict runs
		// pin their target and verify it above.
		if sctx.Run.Shipping == nil {
			if reader, ok := host.(scm.PRBaseBranchReader); ok {
				live, readErr := reader.GetPRBaseBranch(ctx, pr)
				if readErr != nil && pluginPollFailsStep(readErr) {
					clearCIMonitorReady(sctx)
					return nil, readErr
				}
				if readErr != nil {
					// An unreadable target cannot be classified against; wait
					// for the next poll instead of using a possibly stale one.
					sctx.Log(fmt.Sprintf("warning: could not read the PR target branch: %v", readErr))
					clearCIMonitorReady(sctx)
					lastMonitorLog = ""
					if err := waitForPoll(); err != nil {
						return nil, err
					}
					continue
				}
				if live = strings.TrimSpace(live); live != "" && live != baseBranch {
					sctx.Log(fmt.Sprintf("PR target changed from %s to %s, re-reading its required checks", baseBranch, live))
					baseBranch = live
					pr.BaseBranch = live
					checkScope = newCIChecksScope(sctx, host, live)
					// Timeout evidence judged the old target; it must not be
					// reported before a poll under the new scope replaces it.
					timeoutFailingChecks = nil
					timeoutMergeConflict = false
					lastScopeLog = ""
					lastMonitorLog = ""
					clearCIMonitorReady(sctx)
					// Re-record the scope this poll classifies under, and stop
					// if it cannot be: nothing may be classified or repaired
					// under a scope the approval and receipt paths do not see.
					resolutionRecorded = false
					if err := record(sctx, checkScope); err != nil {
						return nil, err
					}
				}
			}
		}

		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Re-arm the timeout whenever the base branch advances.
		if !unlimited {
			resolveWindow := defaultBaseBranchTipResolveWindow
			if remaining := timeout - now().Sub(timeoutAnchor); remaining <= 0 {
				return timeoutOutcome()
			} else if remaining < resolveWindow {
				resolveWindow = remaining
			}
			tipCtx, cancel := context.WithTimeout(ctx, resolveWindow)
			tip, resolved := baseBranchTip(tipCtx)
			cancel()
			if resolved && tip != "" {
				if lastBaseTip == "" {
					lastBaseTip = tip
				} else if tip != lastBaseTip {
					sctx.Log(fmt.Sprintf("base branch advanced (%s..%s), re-arming CI monitor timeout", shortSHA(lastBaseTip), shortSHA(tip)))
					timeoutAnchor = now()
					lastBaseTip = tip
				}
			}
		}

		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Check PR state (merged/closed -> exit)
		prStateKnown := true
		state, err := host.GetPRState(ctx, pr)
		if err != nil && pluginPollFailsStep(err) {
			clearCIMonitorReady(sctx)
			return nil, err
		}
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
			prStateKnown = false
		} else if terminal, done, terminalErr := terminalCIOutcome(sctx, host, pr, state); done || terminalErr != nil {
			return terminal, terminalErr
		} else if state == scm.PRStateOpen {
			if err := observeCIState(sctx, "open"); err != nil {
				return nil, err
			}
		}
		if sctx.Run.Shipping != nil {
			if err := shipping.CheckImmutable(ctx, sctx.Run, sctx.Repo, sctx.WorkDir); err != nil {
				return nil, err
			}
			comparison, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
			if err != nil {
				return nil, err
			}
			if comparison == nil || !comparison.Approved {
				return nil, fmt.Errorf("strict CI lacks approved review comparison")
			}
			upstreamURL := git.ResolveUpstreamURL(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Repo.URLsVerified)
			tip, err := shipping.RemoteHead(ctx, sctx.WorkDir, upstreamURL, comparison.TargetBranch)
			if err != nil {
				return nil, err
			}
			if tip != comparison.TargetSHA {
				if err := setCIMonitorReadiness(sctx, false, false); err != nil {
					return nil, err
				}
				return &pipeline.StepOutcome{RestartFrom: types.StepRebase}, nil
			}
		}

		// Check mergeable state if the provider supports it
		mergeConflict := false
		mergeBehind := false
		mergeabilityKnown := true
		if host.Capabilities().MergeableState {
			mergeState, mergeErr := host.GetMergeableState(ctx, pr)
			if mergeErr != nil && pluginPollFailsStep(mergeErr) {
				clearCIMonitorReady(sctx)
				return nil, mergeErr
			}
			if mergeErr != nil {
				sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", mergeErr))
				mergeabilityBlockedReason = ""
				mergeabilityKnown = false
			} else {
				mergeConflict = mergeState.Conflict()
				// Unless ci.rebase_behind opts in, BEHIND reads as MERGEABLE:
				// the green PR reports checks-passed and waits for a human.
				mergeBehind = mergeState.Behind() && sctx.Config != nil && sctx.Config.CI.RebaseBehind
				mergeabilityKnown = mergeState.Resolved()
				if !mergeabilityKnown {
					sctx.Log(fmt.Sprintf("mergeable state still pending: %s", mergeState))
					mergeabilityBlockedReason = fmt.Sprintf("PR mergeability remained unresolved before timeout: %s", mergeState)
				} else {
					mergeabilityBlockedReason = ""
					timeoutMergeConflict = mergeConflict
				}
			}
		}

		// Check CI status - wait for in-scope executions before escalating
		pr.HeadSHA = sctx.Run.HeadSHA
		checks, err := host.GetChecks(ctx, pr)
		if err == nil {
			err = scm.ValidateCheckHeads(checks, pr.HeadSHA)
		}
		if err != nil && pluginPollFailsStep(err) {
			clearCIMonitorReady(sctx)
			return nil, err
		}
		if err != nil {
			if errors.Is(err, scm.ErrHeadChanged) {
				timeoutFailingChecks = nil
				timeoutMergeConflict = false
				// The provider answered, but nothing it offered is evidence for
				// the commit this run is delivering: no pipeline has registered
				// for that commit yet, and the pipeline the merge request still
				// points at belongs to an older one. That state resolves when
				// the provider creates the pipeline, so it is a wait, not a
				// read failure - counting it toward the consecutive-error limit
				// would park the run after a few minutes telling the operator to
				// check a CLI that answered fine. The idle timeout still bounds
				// the wait, and the readiness signal is cleared because the
				// previous observation was for a different head.
				consecutiveCheckErrs = 0
				lastMonitorLog = logCIMonitorStatus(sctx, fmt.Sprintf("%s%v", cimonitor.ChecksHeadWaitingPrefix, err), lastMonitorLog)
			} else {
				clearCIMonitorReady(sctx)
				lastMonitorLog = ""
				sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
				consecutiveCheckErrs++
				// A provider read that keeps failing (e.g. gh < v2.50 rejecting
				// `gh pr checks --json`) must become an actionable stop, not an
				// invisible spin to ci_timeout. The PR state check above returned
				// already for merged/closed, so reaching here means the PR is open.
				if consecutiveCheckErrs >= consecutiveCheckErrorLimit {
					sctx.Log(fmt.Sprintf("CI checks could not be read %d consecutive times, parking for a decision", consecutiveCheckErrs))
					return ciTerminalRepairOutcome(ciCheckReadFailureOutcome(err), Findings{}, sctx.DeferredFindings), nil
				}
			}
		} else {
			consecutiveCheckErrs = 0
			// Scope repair work before classifying failures. Informational and
			// external checks are logged separately; ScopedChecks.Settled and
			// reviewBotPresenceMarkers preserve the review-bot comment policy.
			rollupObserved := len(checks) > 0
			scoped := checkScope.Apply(ctx, pr, checks)
			if err := recordResolution(); err != nil {
				return nil, err
			}
			checks = scoped.Relevant
			excludedChecks := append(scoped.External, scoped.Informational...)
			if err := s.scopeRetainedCIReruns(sctx, checkScope, pr, checks, excludedChecks); err != nil {
				return nil, err
			}
			if err := scopeRetainedCIFindings(sctx, checkScope, pr, append(append([]scm.Check(nil), checks...), excludedChecks...)); err != nil {
				return nil, err
			}
			// A configured exclusion of a review bot's check also covers the bot
			// before it registers one, so the absent-bot fallback stays off.
			excludedChecks = scoped.ReviewBotPresenceMarkers(checkScope.ExternalReviewBotChecks())
			if summary := scoped.Summary(); summary != lastScopeLog {
				lastScopeLog = summary
				if summary != "" {
					sctx.Log("CI checks not evaluated: " + summary)
				}
			}
			// A failure the provider produced before the repository's own steps
			// ran (a setup/action-resolution outage) is infrastructure, not a
			// verdict on the code. Re-bucket those into the transient path before
			// anything reads the failing set, so they are re-run rather than sent
			// to the fix agent. Gated on the transient budget, so an opted-out
			// repo pays no extra provider calls and keeps the prior behavior.
			markPreRunInfraFailures(sctx, host, checks)
			// checksPending is the narrow execution state: only checks that are
			// actively running or queued block a rerun or issue escalation. A
			// provider-cancelled check is terminal enough to enter the transient
			// rerun policy, even though it is not a verdict on the code. A
			// check held for maintainer approval will not finish on its own,
			// so it does not defer the other checks' issues.
			checksPending := hasExecutingPendingChecks(checks)
			// readinessPending is deliberately broader: any state that is not a
			// conclusive pass, failure, or skip must keep the PR non-ready. This
			// includes cancelled, held, and unknown provider states.
			readinessPending := !scoped.Settled()
			failing := failingCheckNames(checks)

			// A rerun the provider has answered is no longer outstanding. This
			// runs before anything reads the rerun bookkeeping so a resolved
			// rerun cannot be re-opened by a later poll that no longer reports
			// the check, which would park a green head on a cancellation the
			// provider already replaced.
			if _, err := s.retireResolvedReruns(sctx, checks); err != nil {
				sctx.Log(fmt.Sprintf("warning: could not persist the retired rerun state: %v", err))
			}

			// If a terminally failed check completed after our last fix push,
			// CI has already re-run since we pushed (possibly too fast to
			// observe as pending between polls). Treat this as a new iteration
			// so the retry path can fire rather than looping on "fix already
			// attempted" until timeout.
			if terminalFailureCompletedAfter(checks, s.lastFixedCompletedAt) {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
			}

			// Before any failure reaches the fix agent, re-run the checks the
			// provider itself reported as cancelled rather than as a job
			// failure. A rerun costs another provider-side workflow run;
			// escalating one costs an agent round that can edit code which was
			// never broken.
			// Genuine failures never take this path, and a merge conflict is
			// excluded outright: no rerun can ever clear one, so it must reach
			// the fix agent on its first observation.
			rerunIssued := false
			if !checksPending && !mergeConflict {
				issued, rerunOutcome := s.rerunTransientChecks(sctx, host, pr, checks)
				if rerunOutcome != nil {
					// The published head moved, so this run never delivered the
					// commit whose checks were observed: nothing here may leave
					// a ready-to-merge signal behind on the way out.
					clearCIMonitorReady(sctx)
					return rerunOutcome, nil
				}
				rerunIssued = issued
			}
			// A cancelled check is unresolved, not green, and it is not a job
			// failure either: it reaches its own approval gate below rather
			// than the fix agent. A check whose rerun the provider has not
			// published yet is neither, so the monitor keeps waiting for it.
			var unresolvedCancelled, awaitingRerun []string
			if !rerunIssued {
				unresolvedCancelled, awaitingRerun = s.transientReruns.cancelledAfterRerun(foldRunCoveredJobs(checks))
				// A cancelled check this run never re-ran is just as unresolved,
				// and just as final: the provider published a conclusion for it,
				// and with no rerun outstanding nothing this run is waiting on
				// will ever replace it. It has to reach the same gate, or a
				// repository on the default rerun budget of 0 polls a rollup
				// that has already stopped moving until its idle timeout.
				// Checks that can still finish on their own are excluded, so a
				// cancellation observed alongside a running check keeps waiting.
				// Beyond that there is no settling window, for the same reason
				// a genuine failure gets none: a status rollup is per commit,
				// so a cancellation in it belongs to the commit under test and
				// cannot be a leftover from a head this run already replaced.
				//
				// Only the cancel bucket qualifies. A check whose state this
				// version does not recognize is not known to be terminal, so it
				// stays on the wait-then-timeout path rather than being
				// escalated as a conclusion the provider never reported.
				if !checksPending {
					unresolvedCancelled = mergeCheckNames(unresolvedCancelled, s.transientReruns.cancelledWithoutRerun(foldRunCoveredJobs(checks)))
				}
			}
			sort.Strings(failing)
			sort.Strings(unresolvedCancelled)
			sort.Strings(awaitingRerun)
			hasFailures := len(failing) > 0
			hasIssues := hasFailures || mergeConflict || len(unresolvedCancelled) > 0
			// reportedIssues is what the step tells the user about; failing
			// stays the set the fix agent is asked to repair.
			reportedIssues := mergeCheckNames(failing, unresolvedCancelled)
			timeoutFailingChecks = terminalCheckTargetsForNames(checks, mergeCheckNames(reportedIssues, awaitingRerun))

			if hasIssues || len(awaitingRerun) > 0 {
				if err := setCIMonitorReadiness(sctx, false, false); err != nil {
					return nil, err
				}
			}
			if rerunIssued || (!hasIssues && len(awaitingRerun) > 0) {
				// The re-run checks are running again for the same commit, so
				// the monitor waits rather than escalating. This also clears any
				// previous passed-checks signal, which matters for a cancelled
				// check: it never counted as a failing check, so nothing above
				// cleared it.
				lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
			} else if hasIssues && checksPending {
				// Issue handling waits only for checks that can still complete on
				// their own. A cancelled check whose rerun budget is exhausted must
				// reach the approval gate instead of waiting forever.
				lastMonitorLog = ""
				if pendingCheckMatchesLastFixed(checks, s.lastFixedChecks) {
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
				}
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				lastMonitorLog = ""
				if s.lastRepairStillUnverified(checks, mergeConflict) {
					// The provider has not re-run the checks the last
					// published repair targeted: the failures on screen are
					// the ones that repair was for, not a verdict on it.
					// Escalating them again would spend a fix round on stale
					// evidence, so the monitor keeps waiting for the re-run.
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					// No in-scope check is still executing and every authorized
					// rerun is spent: this poll is a settled observation. Each issue
					// becomes one finding carrying its action, and the
					// executor's shared findings machinery decides what
					// happens next - auto-fix findings enter the auto_fix.ci
					// loop as fix rounds, ask-user findings park for a
					// decision, and a poll with only ask-user findings (a
					// transient check no rerun will replace, a review bot's
					// comments) never starts a round.
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
					sctx.DeferredFindings = ""
					s.observedCompletedAt = terminalFailureCompletionTimes(checks)
					findings := ciObservationFindings(ciIssues{
						provider:            host.Provider(),
						checks:              checks,
						failing:             failing,
						unresolvedCancelled: unresolvedCancelled,
						mergeConflict:       mergeConflict,
						reruns:              s.transientReruns.used,
						botComments:         reviewBotComments(sctx, host, pr, checks, excludedChecks...),
						greenBots:           reviewBotCommentsAlways(sctx),
						excludedChecks:      excludedChecks,
					})
					if hasAwaitingApprovalChecks(checks) {
						sctx.Log(ciChecksAwaitingApprovalMsg)
					}
					sctx.Log(fmt.Sprintf("issues detected: %s", findings.Summary))
					return ciObservationOutcome(findings), nil
				}
			} else {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
				switch {
				case !prStateKnown || !mergeabilityKnown:
					clearCIMonitorReady(sctx)
					lastMonitorLog = ""
				case readinessPending:
					// Checks are (re-)running with no failures yet. Surface this
					// so a PR that passed checks and starts re-running clears the
					// previous passed-checks signal instead of looking stale.
					// The broader readiness state is intentional here: cancelled
					// and unknown checks must never be promoted as green.
					// Applies even when no_ci is declared: unresolved in-scope
					// checks are never waived.
					lastMonitorLog = logCIMonitorStatus(sctx, ciWaitingMessage(checks), lastMonitorLog)
				case len(checks) == 0 && (!scoped.Ready() && (sctx.Config == nil || !sctx.Config.NoCI || sctx.Run.Shipping != nil) ||
					// Strict shipping needs an observed rollup even when the
					// target requires only the pipeline's own status.
					!rollupObserved && sctx.Run.Shipping != nil):
					clearCIMonitorReady(sctx)
					lastMonitorLog = ""
					sctx.Log("no CI checks reported yet, waiting for checks to register...")
				case len(checks) == 0 || scoped.Ready():
					// Under ci.review_bot_comments: always, a review bot that
					// concluded its check green, or has not registered one on
					// this head yet, may still have left unresolved comments. They are read before the head is
					// reported ready, and become the same ask-user findings a
					// red bot check produces, so checks-passed is never reported
					// over a comment nobody has looked at. A trusted no_ci head
					// with no checks at all reads them the same way.
					botFindings, botErr := s.greenReviewBotFindings(sctx, host, pr, ciCheckScopePolicy(sctx), checks, excludedChecks...)
					if botErr != nil && pluginPollFailsStep(botErr) {
						clearCIMonitorReady(sctx)
						return nil, botErr
					}
					if botErr != nil {
						clearCIMonitorReady(sctx)
						lastMonitorLog = ""
						botCommentReadFailed = true
						consecutiveBotCommentErrs++
						sctx.Log(fmt.Sprintf("warning: could not read review bot comments: %v", botErr))
						if consecutiveBotCommentErrs >= consecutiveCheckErrorLimit {
							sctx.Log(fmt.Sprintf("review bot comments could not be read %d consecutive times, parking for a decision", consecutiveBotCommentErrs))
							return ciTerminalRepairOutcome(reviewBotCommentsReadFailureOutcome(botErr), Findings{}, sctx.DeferredFindings), nil
						}
						break
					}
					if len(botFindings.Items) > 0 {
						clearCIMonitorReady(sctx)
						sctx.DeferredFindings = ""
						s.observedCompletedAt = terminalFailureCompletionTimes(checks)
						sctx.Log(fmt.Sprintf("issues detected: %s", botFindings.Summary))
						return ciObservationOutcome(botFindings), nil
					}
					if mergeBehind {
						// Green checks the forge still will not merge: the
						// base requires branches to be up to date. The
						// merge-conflict repair rebases onto the PR base.
						behindObserved = true
						tipCtx, cancel := context.WithTimeout(ctx, defaultBaseBranchTipResolveWindow)
						tip, resolved := baseBranchTip(tipCtx)
						cancel()
						if !resolved || tip == "" || tip != behindBaseTip {
							behindBaseTip = ""
							if resolved {
								behindBaseTip = tip
							}
							lastMonitorLog = logCIMonitorStatus(sctx, ciBehindBaseMsg, lastMonitorLog)
							break
						}
						clearCIMonitorReady(sctx)
						sctx.DeferredFindings = ""
						findings := ciObservationFindings(ciIssues{provider: host.Provider(), checks: checks, behindBase: true})
						sctx.Log(fmt.Sprintf("issues detected: %s", findings.Summary))
						return ciObservationOutcome(findings), nil
					}
					sctx.DeferredFindings = ""
					passedMsg := ciChecksPassedMsg
					if len(checks) == 0 && !scoped.Ready() {
						passedMsg = ciNoChecksPassedMsg
					}
					lastMonitorLog = logVerifiedCIMonitorStatus(sctx, host, checks, passedMsg, lastMonitorLog)
					if lastMonitorLog == passedMsg && sctx.PublishedHeadValidated != nil {
						sctx.PublishedHeadValidated(pr.HeadSHA)
					}
				default:
					clearCIMonitorReady(sctx)
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
				}
			}
		}

		if !botCommentReadFailed {
			consecutiveBotCommentErrs = 0
		}
		if !behindObserved {
			behindBaseTip = ""
		}
		if err := waitForPoll(); err != nil {
			return nil, err
		}
	}
}

// ciWaitingMessage names what the monitor is waiting for when no check has
// failed. A workflow the provider is holding for approval has run nothing and
// will run nothing until a maintainer acts, so calling that "checks running"
// describes work that does not exist and hides the one thing that would move
// the run along. Anything else is an ordinary wait on checks in flight.
func ciWaitingMessage(checks []scm.Check) string {
	if hasAwaitingApprovalChecks(checks) {
		return ciChecksAwaitingApprovalMsg
	}
	return ciChecksRunningMsg
}

func logCIMonitorStatus(sctx *pipeline.StepContext, message, previous string) string {
	if message != previous {
		ready := message == ciChecksPassedMsg || message == ciNoChecksPassedMsg
		declaredNoCI := message == ciNoChecksPassedMsg
		if err := setCIMonitorReadiness(sctx, ready, declaredNoCI); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not persist CI readiness: %v", err))
		}
		sctx.Log(message)
	}
	return message
}

func logVerifiedCIMonitorStatus(sctx *pipeline.StepContext, host scm.Host, checks []scm.Check, message, previous string) string {
	if err := verifyCIMonitorComparison(sctx, host, checks); err != nil {
		clearCIMonitorReady(sctx)
		sctx.Log(fmt.Sprintf("warning: CI readiness comparison could not be verified: %v", err))
		return ""
	}
	return logCIMonitorStatus(sctx, message, previous)
}

func verifyCIMonitorComparison(sctx *pipeline.StepContext, host scm.Host, checks []scm.Check) error {
	if sctx != nil && sctx.Run != nil && sctx.Run.Shipping != nil {
		return nil
	}
	if sctx == nil || sctx.Run == nil || sctx.DB == nil || host == nil {
		return fmt.Errorf("CI readiness lacks a run or comparison reader")
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.PRURL == "" || sctx.Run.HeadSHA != receipt.LocalHeadSHA {
		return fmt.Errorf("CI readiness lacks a current PR comparison receipt")
	}
	durable, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return err
	}
	if durable == nil || durable.HeadSHA != receipt.LocalHeadSHA {
		return fmt.Errorf("durable run head differs from CI readiness comparison")
	}
	if err := scm.ValidateCheckHeads(checks, receipt.LocalHeadSHA); err != nil {
		return err
	}
	reader, ok := host.(scm.PRFactsReader)
	if !ok {
		return fmt.Errorf("provider cannot read complete PR facts for CI readiness")
	}
	facts, err := reader.ReadPRFacts(sctx.Ctx, prFromOwnedURL(receipt.PRURL))
	if err != nil {
		return err
	}
	if facts.State != scm.PRStateOpen {
		return fmt.Errorf("PR is no longer open for CI readiness")
	}
	selection := pipeline.PRTargetSelection{PRURL: facts.PR.URL, State: facts.State, SourceRepo: facts.SourceRepository,
		SourceBranch: facts.SourceBranch, ForgeHeadSHA: facts.HeadSHA, TargetBranch: facts.BaseBranch}
	return verifyCurrentPRComparison(sctx, host, selection, receipt, &facts)
}

func clearCIMonitorReady(sctx *pipeline.StepContext) {
	if err := setCIMonitorReadiness(sctx, false, false); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not clear CI readiness: %v", err))
	}
}

func setCIMonitorReadiness(sctx *pipeline.StepContext, ready, declaredNoCI bool) error {
	declaredNoCI = ready && declaredNoCI
	if err := sctx.DB.SetRunCIReadyWithReason(sctx.Run.ID, ready, declaredNoCI); err != nil {
		return err
	}
	if sctx.CIReadinessChanged != nil {
		sctx.CIReadinessChanged(ready, declaredNoCI)
	}
	return nil
}

func notifyPRMerged(sctx *pipeline.StepContext) {
	if sctx == nil || sctx.OnPRMerged == nil || sctx.Run == nil {
		return
	}
	sctx.OnPRMerged(sctx.Ctx, sctx.Run.ID)
}

// Strict custody requires the reviewed PR head, not a cached merged state.
func verifyShippingMergedProof(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	if sctx.Run.Shipping == nil {
		return nil
	}
	reader, ok := host.(scm.ShippingPRFactsReader)
	if !ok {
		return fmt.Errorf("strict shipping requires exact merged PR facts")
	}
	facts, err := reader.GetPRFacts(sctx.Ctx, pr)
	if err != nil {
		return err
	}
	c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
	if err != nil {
		return err
	}
	if c == nil || !c.Approved || c.HeadSHA != sctx.Run.HeadSHA || facts.State != scm.PRStateMerged || facts.URL != pr.URL || facts.HeadSHA != c.HeadSHA || facts.SourceRepo != c.SourceRepo || facts.SourceBranch != c.SourceBranch || facts.TargetBranch != c.TargetBranch || facts.TargetSHA != c.TargetSHA || facts.TargetRepo != github.RepoSlug(sctx.Repo.UpstreamURL) {
		return fmt.Errorf("merged PR differs from approved shipping comparison")
	}
	return shipping.CheckImmutableSource(sctx.Ctx, sctx.Run, sctx.Repo, sctx.WorkDir)
}
