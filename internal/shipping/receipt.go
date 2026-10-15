package shipping

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const MaxPayload = 32 * 1024
const MaxEnvelope = 49 * 1024
const signingDomain = "no-mistakes-review-v1\x00"

type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Algorithm     string `json:"algorithm"`
	KeyID         string `json:"key_id"`
	Payload       string `json:"payload"`
	Signature     string `json:"signature"`
}
type Finding struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        *int   `json:"line"`
	Action      string `json:"action"`
	Description string `json:"description"`
	Disposition string `json:"disposition"`
}
type Review struct {
	Status          string    `json:"status"`
	ApprovedHeadSHA string    `json:"approved_head_sha"`
	FindingsDigest  string    `json:"findings_digest"`
	Findings        []Finding `json:"findings"`
}
type Step struct {
	Step      string `json:"step"`
	Status    string `json:"status"`
	Execution string `json:"execution"`
	// AcceptedGaps carries the owner-approved Test gap this step completed
	// with, re-validated from durable state when the receipt is built.
	AcceptedGaps *db.AcceptedGaps `json:"accepted_gaps,omitempty"`
}
type Payload struct {
	SchemaVersion    int    `json:"schema_version"`
	RunID            string `json:"run_id"`
	Phase            string `json:"phase"`
	IssuedAt         int64  `json:"issued_at"`
	SourceRepo       string `json:"source_repo"`
	SourceBranch     string `json:"source_branch"`
	SubmittedHeadSHA string `json:"submitted_head_sha"`
	LocalHeadSHA     string `json:"local_head_sha"`
	ForgeHeadSHA     string `json:"forge_head_sha"`
	PRURL            string `json:"pr_url"`
	TargetBranch     string `json:"target_branch"`
	TargetSHA        string `json:"target_sha"`
	MergeBaseSHA     string `json:"merge_base_sha"`
	DiffDigest       string `json:"diff_digest"`
	Generation       int64  `json:"generation"`
	IntentDigest     string `json:"intent_digest"`
	ShippingMode     string `json:"shipping_mode"`
	Draft            bool   `json:"draft"`
	Review           Review `json:"review"`
	Steps            []Step `json:"steps"`
	TestOverride     any    `json:"test_override"`
	// checkScope is the CI check scope recorded for the run when a ci-ready
	// proof is built. It is not part of the emitted receipt; it is authority
	// the proof was judged under, so both bracketing snapshots carry it and any
	// movement while the export runs fails it closed.
	checkScope string
}

func Sign(payload []byte, input io.Reader) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("review receipt exceeds payload limit")
	}
	encoded, err := io.ReadAll(io.LimitReader(input, 129))
	if err != nil {
		return nil, fmt.Errorf("read signing seed failed")
	}
	defer clear(encoded)
	if len(encoded) > 128 {
		return nil, fmt.Errorf("signing seed must be base64 of exactly 32 bytes")
	}
	trimmed := bytes.TrimSpace(encoded)
	seed := make([]byte, base64.StdEncoding.DecodedLen(len(trimmed)))
	n, err := base64.StdEncoding.Strict().Decode(seed, trimmed)
	seed = seed[:n]
	if err != nil || len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, fmt.Errorf("signing seed must be base64 of exactly 32 bytes")
	}
	defer clear(seed)
	key := ed25519.NewKeyFromSeed(seed)
	defer clear(key)
	signature := ed25519.Sign(key, append([]byte(signingDomain), payload...))
	defer clear(signature)
	public := key.Public().(ed25519.PublicKey)
	out, err := json.Marshal(Envelope{1, "Ed25519", Digest(public), base64.StdEncoding.EncodeToString(payload), base64.StdEncoding.EncodeToString(signature)})
	if err != nil {
		return nil, err
	}
	if len(out) > MaxEnvelope {
		return nil, fmt.Errorf("review receipt exceeds envelope limit")
	}
	return out, nil
}

// Export constructs a receipt from durable pipeline authority and brackets its
// source/target facts. It does not consume caller JSON or mutate run state.
func Export(ctx context.Context, database *db.DB, repo *db.Repo, runID, phase string, host scm.Host) ([]byte, error) {
	if safeurl.Redact(repo.UpstreamURL) != repo.UpstreamURL && strings.HasPrefix(repo.UpstreamURL, "http") || safeurl.Redact(repo.PushURL()) != repo.PushURL() && strings.HasPrefix(repo.PushURL(), "http") {
		return nil, fmt.Errorf("credential-bearing shipping URL is refused")
	}
	if phase != "review" && phase != "ci-ready" {
		return nil, fmt.Errorf("receipt phase must be review or ci-ready")
	}
	reader, ok := host.(scm.ShippingPRFactsReader)
	if !ok {
		return nil, fmt.Errorf("exact shipping proof is unsupported by this provider")
	}
	first, err := durablePayload(database, runID, phase)
	if err != nil {
		return nil, err
	}
	if safeurl.Redact(first.PRURL) != first.PRURL {
		return nil, fmt.Errorf("credential-bearing PR URL is refused")
	}
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, err
	}
	dir := run.WorktreePath()
	if dir == "" {
		return nil, fmt.Errorf("shipping proof lacks recorded worktree")
	}
	pr := &scm.PR{URL: first.PRURL, HeadSHA: first.LocalHeadSHA}
	pr.Number, err = scm.ExtractPRNumber(pr.URL)
	if err != nil {
		return nil, err
	}
	facts, err := reader.GetPRFacts(ctx, pr)
	if err != nil {
		return nil, err
	}
	if err := checkFacts(first, repo, facts, run.Shipping.Draft); err != nil {
		return nil, err
	}
	if run.Shipping.Draft == nil {
		first.Draft = facts.Draft
	}
	if err := checkGit(ctx, dir, repo, first); err != nil {
		return nil, err
	}
	if phase == "ci-ready" {
		checks, err := host.GetChecks(ctx, pr)
		if err != nil {
			return nil, err
		}
		if pr.HeadSHA != first.LocalHeadSHA {
			return nil, fmt.Errorf("PR head moved during readiness observation")
		}
		// Readiness is judged on the same checks the CI step judged it on:
		// the scope it recorded for this run, not every context on the head.
		policy, err := parseRecordedCheckScope(first.checkScope)
		if err != nil {
			return nil, err
		}
		scope := scm.NewCheckScope(host, policy, first.TargetBranch, nil)
		scoped := scope.Apply(ctx, pr, checks)
		if len(checks) == 0 || len(scoped.Relevant) == 0 && !scoped.Ready() {
			return nil, fmt.Errorf("strict readiness requires live checks")
		}
		if !scoped.Ready() {
			return nil, fmt.Errorf("strict readiness requires all live checks green")
		}
		comments, err := scm.ReadReadinessReviewBotComments(ctx, host, pr, policy, scoped.Relevant, scoped.ReviewBotPresenceMarkers(scope.ExternalReviewBotChecks())...)
		if err != nil {
			return nil, fmt.Errorf("could not verify review bot comments: %w", err)
		}
		if len(comments) > 0 {
			return nil, fmt.Errorf("strict readiness requires no unresolved review bot comments")
		}
	}
	afterFacts, err := reader.GetPRFacts(ctx, pr)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(facts, afterFacts) {
		return nil, fmt.Errorf("PR comparison moved during receipt export")
	}
	if err := checkGit(ctx, dir, repo, first); err != nil {
		return nil, err
	}
	after, err := durablePayload(database, runID, phase)
	if err != nil {
		return nil, err
	}
	if run.Shipping.Draft == nil {
		after.Draft = facts.Draft
	}
	if !reflect.DeepEqual(first, after) {
		return nil, fmt.Errorf("pipeline authority moved during receipt export")
	}
	first.IssuedAt = time.Now().Unix()
	out, err := json.Marshal(first)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxPayload {
		return nil, fmt.Errorf("review receipt exceeds payload limit")
	}
	return out, nil
}

// parseRecordedCheckScope interprets the check-scope record the proof's
// authority snapshot carried. A run that recorded none predates check scoping,
// so it keeps the legacy behavior of judging every check rather than the auto
// default.
func parseRecordedCheckScope(raw string) (scm.CheckScopePolicy, error) {
	var policy scm.CheckScopePolicy
	if raw == "" {
		return scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll}, nil
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, fmt.Errorf("invalid recorded CI check scope")
	}
	return policy, nil
}

func durablePayload(database *db.DB, runID, phase string) (Payload, error) {
	p := Payload{}
	r, err := database.GetRun(runID)
	if err != nil {
		return p, err
	}
	if r == nil || r.Shipping == nil || r.Status != types.RunRunning || r.PushActive || r.SubmittedHeadSHA == nil || r.PRURL == nil || r.Intent == nil || strings.TrimSpace(*r.Intent) == "" {
		return p, fmt.Errorf("strict shipping run is not ready for a receipt")
	}
	if err := r.Shipping.Validate(); err != nil {
		return p, err
	}
	c, err := database.GetReviewComparison(runID)
	if err != nil {
		return p, err
	}
	if c == nil || !c.Approved || c.Generation < 1 || c.HeadSHA != r.HeadSHA || r.ReviewApprovedHeadSHA == nil || *r.ReviewApprovedHeadSHA != r.HeadSHA {
		return p, fmt.Errorf("current exact review comparison is not approved")
	}
	if c.SourceBranch != strings.TrimPrefix(r.Branch, "refs/heads/") {
		return p, fmt.Errorf("source branch differs from review comparison")
	}
	if r.Shipping.Mode == types.ShippingImmutableSource && *r.SubmittedHeadSHA != r.HeadSHA {
		return p, fmt.Errorf("immutable submitted source changed")
	}
	if r.Shipping.Draft != nil {
		p.Draft = *r.Shipping.Draft
	}
	if r.Shipping.Mode == types.ShippingFull && (r.LastPushedSHA == nil || *r.LastPushedSHA != r.HeadSHA || r.PushGeneration == nil || *r.PushGeneration < 1 || r.PushRef == nil || *r.PushRef != "refs/heads/"+c.SourceBranch || r.PushTargetFingerprint == nil || *r.PushTargetFingerprint == "" || r.PushTargetKind == nil) {
		return p, fmt.Errorf("strict shipping lacks exact durable publication proof")
	}
	if phase == "ci-ready" && (r.CIReadyAt == nil || r.CIReadyNoCI) {
		return p, fmt.Errorf("CI readiness has not settled for this run")
	}
	rows, err := database.GetStepsByRun(runID)
	if err != nil {
		return p, err
	}
	gatesJSON, err := database.GetRunGates(runID)
	if err != nil {
		return p, err
	}
	gates, err := config.ParseGates(gatesJSON)
	if err != nil {
		return p, err
	}
	expected := []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	sequence := make([]types.StepName, 0, len(expected)+len(gates))
	for _, step := range expected {
		sequence = append(sequence, step)
		for _, gate := range gates {
			if gate.After == step {
				sequence = append(sequence, gate.StepName())
			}
		}
	}
	if len(rows) != len(sequence) {
		return p, fmt.Errorf("strict shipping requires every configured step")
	}
	proofSteps := make([]Step, 0, len(rows))
	reviewFindings := make([]Finding, 0)
	for i, row := range rows {
		want := types.StepStatusCompleted
		if row.StepName == types.StepCI {
			want = types.StepStatusRunning
		}
		noSurfaceAcknowledged := row.ApprovalReason != nil && row.NoSurfaceTestAcknowledgmentEligible()
		var acceptedGaps *db.AcceptedGaps
		if row.StepName == types.StepTest {
			acceptedGaps = row.ValidAcceptedGaps()
		}
		if row.StepName != sequence[i] || row.Status != want || row.StepName == types.StepTest && row.StrictApprovalRefusal("") != "" || row.SkipReason != nil || row.OverrideReason != nil || row.ApprovalReason != nil && !noSurfaceAcknowledged || row.ExitCode != nil && *row.ExitCode != 0 {
			return p, fmt.Errorf("shipping step is incomplete, skipped, or overridden: %s", row.StepName)
		}
		execution := "performed"
		if r.Shipping.Mode == types.ShippingImmutableSource && (row.StepName == types.StepRebase || row.StepName == types.StepPush) {
			execution = "verified-immutable"
		}
		proofSteps = append(proofSteps, Step{string(row.StepName), string(row.Status), execution, acceptedGaps})
		if row.FindingsJSON != nil {
			findings, err := types.ParseFindingsJSON(*row.FindingsJSON)
			if err != nil {
				return p, fmt.Errorf("invalid durable shipping findings")
			}
			if row.StepName == types.StepTest && findings.Verdict == types.TestVerdictNoSurface && !noSurfaceAcknowledged {
				return p, fmt.Errorf("shipping no-surface Test lacks a valid acknowledgment")
			}
			if types.HasActionableFindings(findings) && !noSurfaceAcknowledged && acceptedGaps == nil {
				return p, fmt.Errorf("shipping findings remain actionable: %s", row.StepName)
			}
			if row.StepName == types.StepReview {
				for _, f := range findings.Items {
					if f.Action != types.ActionNoOp || !types.IsKnownFindingSeverity(f.Severity) || strings.TrimSpace(f.Description) == "" || f.Line < 0 {
						return p, fmt.Errorf("invalid durable review disposition")
					}
					var line *int
					if f.Line > 0 {
						v := f.Line
						line = &v
					}
					reviewFindings = append(reviewFindings, Finding{f.ID, f.Severity, f.File, line, f.Action, f.Description, "non-actionable"})
				}
			}
		}
	}
	findingBytes, _ := json.Marshal(reviewFindings)
	checkScope := ""
	if phase == "ci-ready" {
		if checkScope, err = database.GetRunCheckScope(runID); err != nil {
			return p, err
		}
	}
	p = Payload{SchemaVersion: 1, RunID: runID, Phase: phase, SourceRepo: c.SourceRepo, SourceBranch: c.SourceBranch, SubmittedHeadSHA: *r.SubmittedHeadSHA, LocalHeadSHA: r.HeadSHA, ForgeHeadSHA: r.HeadSHA, PRURL: *r.PRURL, TargetBranch: c.TargetBranch, TargetSHA: c.TargetSHA, MergeBaseSHA: c.MergeBaseSHA, DiffDigest: c.DiffDigest, Generation: c.Generation, IntentDigest: Digest([]byte(*r.Intent)), ShippingMode: r.Shipping.Mode, Draft: p.Draft, Review: Review{"completed", r.HeadSHA, Digest(findingBytes), reviewFindings}, Steps: proofSteps, checkScope: checkScope}
	return p, nil
}

func checkFacts(p Payload, repo *db.Repo, f scm.PRFacts, draft *bool) error {
	if f.State != scm.PRStateOpen || f.URL != p.PRURL || f.SourceRepo != p.SourceRepo || f.SourceRepo != github.RepoSlug(repo.PushURL()) || f.SourceBranch != p.SourceBranch || f.HeadSHA != p.LocalHeadSHA || f.TargetRepo != github.RepoSlug(repo.UpstreamURL) || f.TargetBranch != p.TargetBranch || f.TargetSHA != p.TargetSHA || draft != nil && f.Draft != *draft {
		return fmt.Errorf("live PR comparison differs from approved Review")
	}
	return nil
}
func checkGit(ctx context.Context, dir string, repo *db.Repo, p Payload) error {
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return err
	}
	status, err := git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if head != p.LocalHeadSHA || strings.TrimSpace(status) != "" {
		return fmt.Errorf("pipeline worktree differs from reviewed head")
	}
	pushURL := git.ResolvePushURL(ctx, dir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
	source, err := RemoteHead(ctx, dir, pushURL, p.SourceBranch)
	if err != nil {
		return err
	}
	upstreamURL := git.ResolveUpstreamURL(ctx, dir, repo.UpstreamURL, repo.URLsVerified)
	target, err := RemoteHead(ctx, dir, upstreamURL, p.TargetBranch)
	if err != nil {
		return err
	}
	if source != p.LocalHeadSHA || target != p.TargetSHA {
		return fmt.Errorf("live source or target moved after Review")
	}
	base, diff, err := Compare(ctx, dir, p.TargetSHA, p.LocalHeadSHA)
	if err != nil {
		return err
	}
	if p.ShippingMode == types.ShippingFull && base != p.TargetSHA {
		return fmt.Errorf("strict full shipping target is absent from reviewed head; rerun Rebase")
	}
	if base != p.MergeBaseSHA || diff != p.DiffDigest {
		return fmt.Errorf("raw reviewed comparison changed")
	}
	return nil
}
