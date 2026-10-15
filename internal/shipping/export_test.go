package shipping

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type receiptForge struct {
	scm.Host
	facts   scm.PRFacts
	checks  []scm.Check
	observe func()
}

func (h *receiptForge) GetPRFacts(context.Context, *scm.PR) (scm.PRFacts, error) {
	if h.observe != nil {
		h.observe()
	}
	return h.facts, nil
}
func (h *receiptForge) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) { return h.checks, nil }

type exportFixture struct {
	database *db.DB
	repo     *db.Repo
	run      *db.Run
	forge    *receiptForge
	dir      string
}

func receiptGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	binary, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func newExportFixture(t *testing.T) *exportFixture {
	return newExportFixtureWithDraft(t, nil)
}

func newExportFixtureWithDraft(t *testing.T, draft *bool) *exportFixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "worktree")
	remote := filepath.Join(root, "origin.git")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	receiptGit(t, root, "init", "--bare", remote)
	receiptGit(t, dir, "init", "-b", "main")
	receiptGit(t, dir, "config", "user.email", "fixture@example.test")
	receiptGit(t, dir, "config", "user.name", "Fixture")
	receiptGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	receiptGit(t, dir, "add", "source.txt")
	receiptGit(t, dir, "commit", "-m", "base")
	base := receiptGit(t, dir, "rev-parse", "HEAD")
	receiptGit(t, dir, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatal(err)
	}
	receiptGit(t, dir, "commit", "-am", "feature")
	head := receiptGit(t, dir, "rev-parse", "HEAD")
	receiptGit(t, dir, "push", remote, "main", "feature")
	url := "https://github.com/owner/project.git"
	receiptGit(t, dir, "config", "url."+remote+".insteadOf", url)
	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(dir, url, "main")
	if err != nil {
		t.Fatal(err)
	}
	intent := &db.RunIntent{Summary: "exact user intent\n", Source: db.RunIntentSourceAgent, Score: 1}
	run, err := database.InsertRunWithShipping(repo.ID, "feature", head, base, intent, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, Draft: draft})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/owner/project/pull/7"
	if err := database.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	_, digest, err := Compare(context.Background(), dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "owner/project", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		sr, err := database.InsertStepResult(run.ID, step)
		if err != nil {
			t.Fatal(err)
		}
		if step == types.StepReview {
			err = database.CompleteReviewStep(sr.ID, run.ID, head, 0, 1, "", 1)
		} else if step == types.StepCI {
			err = database.UpdateStepStatus(sr.ID, types.StepStatusRunning)
		} else {
			err = database.CompleteStep(sr.ID, 0, 1, "")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: Digest([]byte(url)), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	forge := &receiptForge{facts: scm.PRFacts{URL: prURL, State: scm.PRStateOpen, SourceRepo: "owner/project", SourceBranch: "feature", HeadSHA: head, TargetRepo: "owner/project", TargetBranch: "main", TargetSHA: base}, checks: []scm.Check{{Name: "approval", Bucket: scm.CheckBucketPending}}}
	return &exportFixture{database, repo, run, forge, dir}
}

func TestExportRefusesPinnedDraftDrift(t *testing.T) {
	draft := false
	f := newExportFixtureWithDraft(t, &draft)
	f.forge.facts.Draft = true
	if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge); err == nil {
		t.Fatal("draft drift accepted")
	}
}

// The receipt contract records the live PR draft state when it matches the
// explicit run pin. A ready-for-review proof for a pinned draft PR is false.
func TestExportPreservesPinnedDraftInReceipt(t *testing.T) {
	draft := true
	f := newExportFixtureWithDraft(t, &draft)
	f.forge.facts.Draft = true
	raw, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge)
	if err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if !p.Draft {
		t.Fatal("receipt claims draft=false for the approved, explicitly pinned draft PR")
	}
	signed, err := Sign(raw, strings.NewReader(base64.StdEncoding.EncodeToString(make([]byte, 32))))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(signed, &envelope); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var signedPayload Payload
	if err := json.Unmarshal(payload, &signedPayload); err != nil {
		t.Fatal(err)
	}
	if !signedPayload.Draft {
		t.Fatal("signed receipt claims draft=false for the approved, explicitly pinned draft PR")
	}
}

func TestExportReviewBeforeApprovalThenExactCIReady(t *testing.T) {
	f := newExportFixture(t)
	ctx := context.Background()
	raw, err := Export(ctx, f.database, f.repo, f.run.ID, "review", f.forge)
	if err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != "review" || p.Generation != 1 || p.Steps[len(p.Steps)-1].Status != "running" || p.IntentDigest != Digest([]byte("exact user intent\n")) {
		t.Fatalf("wrong review proof: %+v", p)
	}
	if p.Review.Findings == nil {
		t.Fatal("empty findings emitted null")
	}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", f.forge); err == nil {
		t.Fatal("pending CI claimed ready")
	}
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	f.forge.checks = []scm.Check{{Name: "approval", Bucket: scm.CheckBucketPass}, {Name: "unaffected optional lane", Bucket: scm.CheckBucketSkip}}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", f.forge); err != nil {
		t.Fatal(err)
	}
	f.forge.checks = nil
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", f.forge); err == nil {
		t.Fatal("empty checks accepted")
	}
}

func TestExportRefusesRacesOverridesSkipsAndActionableFindings(t *testing.T) {
	for _, kind := range []string{"generation", "head", "target", "override", "skip", "actionable", "publication", "dirty"} {
		t.Run(kind, func(t *testing.T) {
			f := newExportFixture(t)
			rows, _ := f.database.GetStepsByRun(f.run.ID)
			switch kind {
			case "generation":
				f.forge.observe = func() {
					c, _ := f.database.GetReviewComparison(f.run.ID)
					if _, err := f.database.CaptureReviewComparison(f.run.ID, *c); err != nil {
						t.Fatal(err)
					}
				}
			case "head":
				f.forge.facts.HeadSHA = strings.Repeat("a", 40)
			case "target":
				f.forge.facts.TargetSHA = strings.Repeat("b", 40)
			case "override":
				if err := f.database.SetStepOverrideReason(rows[3].ID, "not authorized"); err != nil {
					t.Fatal(err)
				}
			case "skip":
				if err := f.database.UpdateStepStatus(rows[6].ID, types.StepStatusSkipped); err != nil {
					t.Fatal(err)
				}
			case "actionable":
				if err := f.database.SetStepFindings(rows[2].ID, `{"summary":"remaining","items":[{"id":"f1","severity":"error","description":"bug","action":"ask-user"}]}`); err != nil {
					t.Fatal(err)
				}
			case "publication":
				if err := f.database.UpdateRunPushBinding(f.run.ID, db.PushBinding{HeadSHA: strings.Repeat("a", 40), Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.dir, "dirty"), []byte("unreviewed"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge); err == nil {
				t.Fatal("invalid proof accepted")
			}
		})
	}
}

func TestExportFullRefusesUnintegratedCurrentTarget(t *testing.T) {
	f := newExportFixture(t)
	receiptGit(t, f.dir, "switch", "main")
	if err := os.WriteFile(filepath.Join(f.dir, "target-only"), []byte("advance"), 0644); err != nil {
		t.Fatal(err)
	}
	receiptGit(t, f.dir, "add", "target-only")
	receiptGit(t, f.dir, "commit", "-m", "advance target")
	target := receiptGit(t, f.dir, "rev-parse", "HEAD")
	receiptGit(t, f.dir, "push", f.repo.UpstreamURL, "main")
	receiptGit(t, f.dir, "switch", "feature")
	base, digest, err := Compare(context.Background(), f.dir, target, f.run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.database.CaptureReviewComparison(f.run.ID, db.ReviewComparison{SourceRepo: "owner/project", SourceBranch: "feature", TargetBranch: "main", TargetSHA: target, HeadSHA: f.run.HeadSHA, MergeBaseSHA: base, DiffDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := f.database.GetStepsByRun(f.run.ID)
	if err := f.database.CompleteReviewStep(rows[2].ID, f.run.ID, f.run.HeadSHA, 0, 1, "", c.Generation); err != nil {
		t.Fatal(err)
	}
	f.forge.facts.TargetSHA = target
	if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge); err == nil {
		t.Fatal("full proof admitted target absent from reviewed head")
	}
}

func TestExportNoSurfaceAcknowledgment(t *testing.T) {
	const evidence = `{"findings":[{"id":"test-1","severity":"warning","action":"ask-user","description":"this change has no live-validatable surface; proceed without live validation? (0 of 1 scenarios were driven live against the product); documentation: Markdown has no running product surface"}],"verdict":"no-surface","scenarios":[{"name":"documentation","result":"untested","live":false,"reason":"Markdown has no running product surface"}]}`
	for _, reason := range []string{"", "Documentation correctness confirmed"} {
		for _, kind := range []string{"genuine", "unacknowledged", "no-go", "inconclusive", "live", "pass", "fail", "empty", "missing-reason", "missing-finding", "lookalike", "unrelated", "budget", "command-finding", "command-exit", "override", "malformed"} {
			t.Run(kind+"/"+reason, func(t *testing.T) {
				f := newExportFixture(t)
				rows, err := f.database.GetStepsByRun(f.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				sr := rows[3]
				findings, err := types.ParseFindingsJSON(evidence)
				if err != nil {
					t.Fatal(err)
				}
				exit := 0
				switch kind {
				case "no-go":
					findings.Verdict = types.TestVerdictNoGo
					findings.Items = nil
				case "inconclusive":
					findings.Verdict = types.TestVerdictInconclusive
					findings.Items = nil
				case "live":
					findings.Scenarios[0].Live = true
				case "pass":
					findings.Scenarios[0].Result = types.ScenarioResultPass
				case "fail":
					findings.Scenarios[0].Result = types.ScenarioResultFail
				case "empty":
					findings.Scenarios = nil
				case "missing-reason":
					findings.Scenarios[0].Reason = ""
				case "missing-finding":
					findings.Items = nil
				case "lookalike":
					findings.Items[0].Source = types.FindingSourceUser
				case "unrelated":
					findings.Items = append(findings.Items, types.Finding{ID: "other", Severity: "error", Action: "ask-user", Description: "Unresolved defect"})
				case "budget":
					findings.Items = append(findings.Items, types.Finding{ID: types.FindingIDTestAgentTimeout, Severity: "warning", Action: "no-op", Description: "Invocation cut"})
				case "command-finding":
					findings.Items = append(findings.Items, types.Finding{ID: "command", Severity: "error", Category: types.FindingCategoryTestCommand, Action: "no-op", Description: "Failed command"})
				case "command-exit":
					exit = 1
				}
				raw, err := types.MarshalFindingsJSON(findings)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "malformed" {
					raw = "{"
				}
				if err := f.database.ParkStepForApproval(f.run.ID, sr.ID, types.StepStatusAwaitingApproval, exit, 1, &raw); err != nil {
					t.Fatal(err)
				}
				if kind != "unacknowledged" {
					if err := f.database.SetTestApprovalReason(sr.ID, reason); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "override" {
					if err := f.database.SetStepOverrideReason(sr.ID, "Recorded override"); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.database.CompleteStep(sr.ID, exit, 1, ""); err != nil {
					t.Fatal(err)
				}
				exported, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge)
				if kind != "genuine" {
					if err == nil {
						t.Fatal("invalid Test completion received a strict receipt")
					}
					return
				}
				if err != nil {
					t.Fatalf("acknowledged no-surface refused receipt: %v", err)
				}
				var payload Payload
				if err := json.Unmarshal(exported, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Steps[3].Status != "completed" || payload.Steps[3].Execution != "performed" {
					t.Fatalf("Test misrepresented in receipt: %+v", payload.Steps[3])
				}
				got, err := f.database.GetStepResult(sr.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.ApprovalReason == nil || *got.ApprovalReason != reason || got.FindingsJSON == nil || *got.FindingsJSON != raw {
					t.Fatalf("receipt export changed acknowledgment evidence: %+v", got)
				}
				if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", f.forge); err == nil {
					t.Fatal("Test acknowledgment waived required hosted CI")
				}
			})
		}
	}
}

func TestExportAcceptedTestGap(t *testing.T) {
	const verdict = `{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: broker timeout"}`
	const evidence = `{"findings":[` + verdict + `],"verdict":"inconclusive"}`
	const reason = "owner approved via first mate, 2026-10-14"
	const coveredBy = "TestBrokerTimeoutMocked"
	for _, kind := range []string{"accepted", "unrecorded", "extra-defect-after-record", "plain-approval-reason", "exit-code"} {
		t.Run(kind, func(t *testing.T) {
			f := newExportFixture(t)
			rows, err := f.database.GetStepsByRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			sr := rows[3]
			raw := evidence
			exit := 0
			if kind == "exit-code" {
				exit = 1
			}
			if err := f.database.ParkStepForApproval(f.run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &raw); err != nil {
				t.Fatal(err)
			}
			if kind != "unrecorded" {
				if _, err := f.database.RecordAcceptedGaps(sr.ID, db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}, true); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "extra-defect-after-record":
				raw = `{"findings":[` + verdict + `,{"id":"test-2","severity":"error","action":"ask-user","description":"defect"}],"verdict":"inconclusive"}`
				if err := f.database.SetStepFindings(sr.ID, raw); err != nil {
					t.Fatal(err)
				}
			case "plain-approval-reason":
				if err := f.database.SetTestApprovalReason(sr.ID, reason); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.database.CompleteStep(sr.ID, exit, 1, ""); err != nil {
				t.Fatal(err)
			}
			exported, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge)
			if kind != "accepted" {
				if err == nil {
					t.Fatal("receipt issued without a valid accepted gap")
				}
				return
			}
			if err != nil {
				t.Fatalf("valid accepted gap refused receipt: %v", err)
			}
			var payload Payload
			if err := json.Unmarshal(exported, &payload); err != nil {
				t.Fatal(err)
			}
			step := payload.Steps[3]
			if step.Status != "completed" || step.Execution != "performed" || step.AcceptedGaps == nil || step.AcceptedGaps.Reason != reason || step.AcceptedGaps.CoveredBy != coveredBy || len(step.AcceptedGaps.FindingIDs) != 1 || step.AcceptedGaps.FindingIDs[0] != "test-verdict" {
				t.Fatalf("receipt step = %+v", step)
			}
			for i, other := range payload.Steps {
				if i != 3 && other.AcceptedGaps != nil {
					t.Fatalf("accepted gaps leaked onto %s", other.Step)
				}
			}
		})
	}
}
