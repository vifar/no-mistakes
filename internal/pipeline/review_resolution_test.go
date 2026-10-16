package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const restoredFinding = `{"findings":[{"id":"review-3","severity":"error","file":"restored.go","description":"selected defect","action":"ask-user"}]}`
const scopeFinding = `{"findings":[{"id":"review-1","severity":"error","file":"current.go","description":"outside accepted intent","action":"ask-user"}]}`

func resolutionFixture(t *testing.T) (*StepContext, *StepOutcome) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("restored.go", "target\n")
	write("current.go", "target\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "target")
	target, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	write("restored.go", "defect\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "introduce defect")
	defective, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	write("restored.go", "target\n")
	write("current.go", "reviewed change\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "restore anchor and retain change")
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	base, digest, err := shipping.Compare(context.Background(), dir, target, head)
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.InsertRunWithShipping(repo.ID, "strict-feature", head, target, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target})
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := database.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "owner/repo", SourceBranch: run.Branch, TargetBranch: "main", TargetSHA: target, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main", TargetSHA: target}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := restoredFinding
	round, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, defective, 1)
	if err != nil {
		t.Fatal(err)
	}
	selection := `["review-3"]`
	if err := database.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database, WorkDir: dir, GateDir: filepath.Dir(p.DB()), StepResultID: step.ID}
	outcome := &StepOutcome{ReviewedPaths: []string{"current.go"}, ReviewablePaths: []string{"current.go"}, ReviewApprovedHeadSHA: head, ReviewComparisonGeneration: comparison.Generation}
	return sctx, outcome
}

func recordScopeDecision(t *testing.T, sctx *StepContext, selection, source string) {
	t.Helper()
	findings := scopeFinding
	round, err := sctx.DB.InsertReviewStepRound(sctx.StepResultID, 2, "auto_fix", &findings, nil, sctx.Run.HeadSHA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selection, source, nil); err != nil {
		t.Fatal(err)
	}
}

// The oracle is exact target-tree equality plus a fresh review of the remaining
// diff. A restored anchor must not strand an independently reviewed generation.
func TestStrictReviewResolutionRestoredAnchor(t *testing.T) {
	sctx, outcome := resolutionFixture(t)
	got, _ := resolveReviewCarry(sctx, restoredFinding, []string{"review-3"}, outcome, `{"findings":[]}`)
	if got != "" {
		t.Fatalf("restored selected anchor remained outstanding: %s", got)
	}
}

// A human decline grants no authority: the independent reviewer must explicitly
// adjudicate it against accepted intent with a reason and complete coverage.
func TestStrictReviewResolutionAcceptedScope(t *testing.T) {
	sctx, outcome := resolutionFixture(t)
	recordScopeDecision(t, sctx, "[]", db.RoundSelectionSourceUser)
	outcome.WithdrawnFindings = []types.WithdrawnFinding{{ID: "review-1", Reason: "Accepted owner requirements exclude this remedy from scope"}}
	got, applied := resolveReviewCarry(sctx, scopeFinding, nil, outcome, `{"findings":[]}`)
	if got != "" || len(applied) != 1 {
		t.Fatalf("independently adjudicated scope decision stayed actionable: %s, %v", got, applied)
	}
}

func TestStrictReviewResolutionFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*testing.T, *StepContext, *StepOutcome)
		current string
	}{
		{name: "missing coverage", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.ReviewedPaths = nil }},
		{name: "partial coverage", change: func(t *testing.T, s *StepContext, o *StepOutcome) {
			o.ReviewablePaths = append(o.ReviewablePaths, "other.go")
		}},
		{name: "fabricated anchor coverage", change: func(t *testing.T, s *StepContext, o *StepOutcome) {
			o.ReviewedPaths = append(o.ReviewedPaths, "restored.go")
		}},
		{name: "malformed coverage", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.ReviewedPaths = append(o.ReviewedPaths, " ") }},
		{name: "missing generation", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.ReviewComparisonGeneration = 0 }},
		{name: "moved generation", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.ReviewComparisonGeneration++ }},
		{name: "moved head", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.ReviewApprovedHeadSHA = strings.Repeat("a", 40) }},
		{name: "missing comparison", change: func(t *testing.T, s *StepContext, o *StepOutcome) { s.Run.ID = "absent" }},
		{name: "unavailable selected object", change: func(t *testing.T, s *StepContext, o *StepOutcome) {
			raw := restoredFinding
			round, err := s.DB.InsertReviewStepRound(s.StepResultID, 2, "auto_fix", &raw, nil, strings.Repeat("a", 40), 1)
			if err != nil {
				t.Fatal(err)
			}
			selection := `["review-3"]`
			if err := s.DB.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unavailable target object", change: func(t *testing.T, s *StepContext, o *StepOutcome) {
			c, err := s.DB.GetReviewComparison(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.TargetSHA = strings.Repeat("a", 40)
			c, err = s.DB.CaptureReviewComparison(s.Run.ID, *c)
			if err != nil {
				t.Fatal(err)
			}
			o.ReviewComparisonGeneration = c.Generation
		}},
		{name: "changed digest", change: func(t *testing.T, s *StepContext, o *StepOutcome) {
			c, err := s.DB.GetReviewComparison(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.DiffDigest = strings.Repeat("a", 64)
			c, err = s.DB.CaptureReviewComparison(s.Run.ID, *c)
			if err != nil {
				t.Fatal(err)
			}
			o.ReviewComparisonGeneration = c.Generation
		}},
		{name: "question gate", change: func(t *testing.T, s *StepContext, o *StepOutcome) { o.NeedsApproval = true }},
		{name: "ordinary run", change: func(t *testing.T, s *StepContext, o *StepOutcome) { s.Run.Shipping = nil }},
		{name: "unreadable current findings", current: "{"},
		{name: "rereported original", current: restoredFinding},
		{name: "other unresolved changed finding", current: scopeFinding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx, outcome := resolutionFixture(t)
			if tc.change != nil {
				tc.change(t, sctx, outcome)
			}
			current := tc.current
			if current == "" {
				current = `{"findings":[]}`
			}
			got, _ := resolveReviewCarry(sctx, restoredFinding, []string{"review-3"}, outcome, current)
			if got != restoredFinding {
				t.Fatalf("unproven resolution: %s", got)
			}
		})
	}
}

func TestStrictReviewResolutionIgnoredChangedAnchor(t *testing.T) {
	sctx, outcome := resolutionFixture(t)
	writeTestFile(t, sctx.WorkDir, "restored.go", "still defective\n")
	execGit(t, sctx.WorkDir, "add", ".")
	execGit(t, sctx.WorkDir, "commit", "-m", "ignored anchor is still changed")
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.HeadSHA = head
	c.MergeBaseSHA, c.DiffDigest, err = shipping.Compare(sctx.Ctx, sctx.WorkDir, c.TargetSHA, head)
	if err != nil {
		t.Fatal(err)
	}
	c, err = sctx.DB.CaptureReviewComparison(sctx.Run.ID, *c)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA, outcome.ReviewApprovedHeadSHA = head, head
	outcome.ReviewComparisonGeneration = c.Generation
	got, _ := resolveReviewCarry(sctx, restoredFinding, []string{"review-3"}, outcome, `{"findings":[]}`)
	if got != restoredFinding {
		t.Fatalf("filtered-but-changed anchor cleared: %s", got)
	}
}

func TestStrictReviewResolutionRemovedLiteralAnchorAndRename(t *testing.T) {
	for _, mode := range []string{"removed", "renamed", "renamed and rewritten"} {
		rename := mode != "removed"
		name := "removed from diff"
		if rename {
			name = "renamed code is retained"
		}
		if mode == "renamed and rewritten" {
			name = mode
		}
		t.Run(name, func(t *testing.T) {
			sctx, outcome := resolutionFixture(t)
			file := ":(glob)introduced*.go"
			writeTestFile(t, sctx.WorkDir, file, "introduced defect\n")
			execGit(t, sctx.WorkDir, "add", ".")
			execGit(t, sctx.WorkDir, "commit", "-m", "introduce anchored file")
			oldHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: "review-4", Severity: types.FindingSeverityError, File: file, Description: "selected introduced defect", Action: types.ActionAskUser}}})
			if err != nil {
				t.Fatal(err)
			}
			round, err := sctx.DB.InsertReviewStepRound(sctx.StepResultID, 2, "auto_fix", &raw, nil, oldHead, 1)
			if err != nil {
				t.Fatal(err)
			}
			selection := `["review-4"]`
			if err := sctx.DB.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceUser, nil); err != nil {
				t.Fatal(err)
			}
			if rename {
				execGit(t, sctx.WorkDir, "--literal-pathspecs", "mv", "--", file, "renamed.go")
				if mode == "renamed and rewritten" {
					writeTestFile(t, sctx.WorkDir, "renamed.go", "rewritten implementation at another anchor\n")
					execGit(t, sctx.WorkDir, "add", ".")
				}
				outcome.ReviewedPaths = append(outcome.ReviewedPaths, "renamed.go")
				outcome.ReviewablePaths = append(outcome.ReviewablePaths, "renamed.go")
			} else {
				execGit(t, sctx.WorkDir, "--literal-pathspecs", "rm", "--", file)
			}
			execGit(t, sctx.WorkDir, "commit", "-m", "resolve literal anchor")
			head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			c.HeadSHA = head
			c.MergeBaseSHA, c.DiffDigest, err = shipping.Compare(sctx.Ctx, sctx.WorkDir, c.TargetSHA, head)
			if err != nil {
				t.Fatal(err)
			}
			c, err = sctx.DB.CaptureReviewComparison(sctx.Run.ID, *c)
			if err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA, outcome.ReviewApprovedHeadSHA = head, head
			outcome.ReviewComparisonGeneration = c.Generation
			got, _ := resolveReviewCarry(sctx, raw, []string{"review-4"}, outcome, `{"findings":[]}`)
			if rename && got != raw {
				t.Fatalf("renamed code was treated as removed: %s", got)
			}
			if !rename && got != "" {
				t.Fatalf("literal restored absence did not clear: %s", got)
			}
		})
	}
}

func TestStrictReviewResolutionScopeRecoveryAndIdentity(t *testing.T) {
	for _, name := range []string{"accepted after reopen", "no independent withdrawal", "no reason", "partial review", "reused id", "selected fix", "auto selection", "operator authored"} {
		t.Run(name, func(t *testing.T) {
			sctx, outcome := resolutionFixture(t)
			selection, source := "[]", db.RoundSelectionSourceUser
			if name == "selected fix" {
				selection = `["review-1"]`
			}
			if name == "auto selection" {
				source = db.RoundSelectionSourceAutoFix
			}
			recordScopeDecision(t, sctx, selection, source)
			// Reopen the persisted database, as a fresh executor does on recovery.
			reopened, err := db.Open(filepath.Join(sctx.GateDir, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			sctx.DB = reopened
			outcome.WithdrawnFindings = []types.WithdrawnFinding{{ID: "review-1", Reason: "Accepted owner requirements exclude this remedy"}}
			raw := scopeFinding
			switch name {
			case "no independent withdrawal":
				outcome.WithdrawnFindings = nil
			case "no reason":
				outcome.WithdrawnFindings[0].Reason = " "
			case "partial review":
				outcome.ReviewedPaths = nil
			case "reused id":
				raw = strings.Replace(raw, "outside accepted intent", "new materially different defect", 1)
			case "operator authored":
				raw = strings.Replace(raw, `"action":"ask-user"`, `"action":"ask-user","source":"user"`, 1)
			}
			got, applied := resolveReviewCarry(sctx, raw, nil, outcome, `{"findings":[]}`)
			if name == "accepted after reopen" {
				if got != "" || len(applied) != 1 {
					t.Fatalf("durable adjudication lost: %s, %v", got, applied)
				}
			} else if got != raw || len(applied) != 0 {
				t.Fatalf("scope disposition waived review or identity: %s, %v", got, applied)
			}
		})
	}
}

func TestStrictReviewResolutionRecoveredGateCertification(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete review"
		if !complete {
			name = "incomplete review remains parked"
		}
		t.Run(name, func(t *testing.T) {
			sctx, outcome := resolutionFixture(t)
			recordScopeDecision(t, sctx, "[]", db.RoundSelectionSourceUser)
			gate := mergeOutstandingFindingsJSON(restoredFinding, scopeFinding, nil)
			if err := sctx.DB.StartStep(sctx.StepResultID); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(sctx.StepResultID, gate); err != nil {
				t.Fatal(err)
			}
			if _, err := sctx.DB.InsertReviewStepRound(sctx.StepResultID, 3, "auto_fix", &gate, nil, sctx.Run.HeadSHA, 1); err != nil {
				t.Fatal(err)
			}
			if err := parkGateFixture(sctx.DB, sctx.StepResultID, types.StepStatusFixReview, 1); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetRunAwaitingAgent(sctx.Run.ID); err != nil {
				t.Fatal(err)
			}
			recovered, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			step := &adaptiveCallStep{name: types.StepReview, fn: func(turn *StepContext) (*StepOutcome, error) {
				parsed, err := types.ParseFindingsJSON(turn.ScopeFindings)
				if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != "review-1" {
					t.Errorf("recovered reviewer lost identity-bound disposition: %s, %v", turn.ScopeFindings, err)
				}
				c, err := turn.DB.GetReviewComparison(turn.Run.ID)
				if err != nil {
					return nil, err
				}
				c, err = turn.DB.CaptureReviewComparison(turn.Run.ID, *c)
				if err != nil {
					return nil, err
				}
				fresh := *outcome
				fresh.Findings = `{"findings":[],"reviewed_paths":["current.go"]}`
				fresh.ReviewComparisonGeneration = c.Generation
				fresh.WithdrawnFindings = []types.WithdrawnFinding{{ID: "review-1", Reason: "Independent trace confirms accepted owner requirements exclude this remedy"}}
				if !complete {
					fresh.ReviewedPaths = nil
					fresh.NeedsApproval = true
				}
				return &fresh, nil
			}}
			e := NewExecutor(sctx.DB, paths.WithRoot(sctx.GateDir), &config.Config{}, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.Resume(ctx, recovered, sctx.Repo, sctx.WorkDir) }()
			wait := func() {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					e.mu.Lock()
					parked := e.waiting
					e.mu.Unlock()
					if parked {
						return
					}
					select {
					case err := <-done:
						t.Fatalf("executor ended before parking: %v", err)
					default:
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatal("review did not park")
			}
			wait()
			if err := e.Respond(types.StepReview, types.ActionApprove, nil, gateSnapshot(e)); err == nil {
				t.Fatal("operator approval granted independent authority")
			}
			if _, err := e.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"review-3"}, []string{"review-1"}, nil, nil, "", gateSnapshot(e)); err != nil {
				t.Fatal(err)
			}
			if complete {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("valid independent review did not complete")
				}
				c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
				if err != nil || !c.Approved || c.HeadSHA != outcome.ReviewApprovedHeadSHA {
					t.Fatalf("missing exact certified comparison: %+v, %v", c, err)
				}
				run, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != c.HeadSHA {
					t.Fatalf("publication authority differs from review: %+v, %v", run, err)
				}
				rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
				if err != nil {
					t.Fatal(err)
				}
				last, err := types.ParseFindingsJSON(*rounds[len(rounds)-1].FindingsJSON)
				if err != nil || len(last.WithdrawnFindings) != 1 || last.WithdrawnFindings[0].Reason == "" {
					t.Fatalf("scope adjudication was not audited: %+v, %v", last, err)
				}
				result, err := sctx.DB.GetStepResult(sctx.StepResultID)
				if err != nil {
					t.Fatal(err)
				}
				stats, err := sctx.DB.StepFindingStats(result)
				if err != nil || stats.ReportedFindings != 2 || stats.FixedFindings != 1 {
					t.Fatalf("scope disposition counted as a code fix: %+v, %v", stats, err)
				}
			} else {
				wait()
				c, err := sctx.DB.GetReviewComparison(sctx.Run.ID)
				if err != nil || c.Approved {
					t.Fatalf("partial review gained comparison authority: %+v, %v", c, err)
				}
				result, err := sctx.DB.GetStepResult(sctx.StepResultID)
				if err != nil || result.FindingsJSON == nil {
					t.Fatalf("outstanding gate lost: %+v, %v", result, err)
				}
				items, err := types.ParseFindingsJSON(*result.FindingsJSON)
				if err != nil || len(items.Items) != 2 {
					t.Fatalf("unproven findings cleared: %+v, %v", items, err)
				}
				if err := e.Respond(types.StepReview, types.ActionAbort, nil, nil); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("aborted unreviewed run passed")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("test executor did not stop")
				}
			}
		})
	}
}

func TestReviewResolutionRecoveredAnswerCannotRetractSelectedFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	result, recovered := seedRecoveredReviewGate(t, database, run, scopeFinding, types.StepStatusFixReview, `["review-1"]`)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.FinalizingAnswers {
			t.Error("fixture did not execute the recovered answer turn")
		}
		return &StepOutcome{NeedsApproval: true, WithdrawnFindings: []types.WithdrawnFinding{{ID: "review-1", Reason: "claim retracted without verifying the selected fix"}}}, nil
	}}
	e := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Resume(ctx, recovered, repo, workDir) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := e.Respond(types.StepReview, types.ActionAnswer, nil, gateSnapshot(e)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered question response did not reach the gate")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		rounds, err := database.GetRoundsByStep(result.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		parked := e.waiting
		e.mu.Unlock()
		if len(rounds) == 2 && parked {
			got, err := database.GetStepResult(result.ID)
			if err != nil || got.FindingsJSON == nil || !outstandingIDs(t, *got.FindingsJSON)["review-1"] {
				t.Fatalf("answer retracted a selected fix without verification: %+v, %v", got, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unverified selected fix did not remain parked")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.Respond(types.StepReview, types.ActionAbort, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unverified review passed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("test executor did not stop")
	}
}
