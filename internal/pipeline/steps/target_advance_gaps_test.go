package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func pipelineReviewFix(t *testing.T, c *pipeline.StepContext) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.WorkDir, "review-fix.txt"), []byte("review fix survives\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(c, types.StepReview, "keep review fix", "fix"); err != nil {
		t.Fatal(err)
	}
}

func integrateTargetRewind(t *testing.T, c *pipeline.StepContext, selected pipeline.PRTargetSelection) {
	t.Helper()
	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil || out == nil || out.NeedsApproval {
		t.Fatalf("integrate rewind: %+v %v", out, err)
	}
	c.PRContextAfterStep, c.PRContextRebaseCompleted = true, true
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextRebaseCompleted = false
}

func TestTargetAdvanceWithSimultaneousReviewFix(t *testing.T) {
	c, selected := rewindFixture(t)
	// G may have adopted the submitted head before the review agent fixed it.
	// Observing that code is not publication of the new, local review fix.
	if err := c.DB.PutCommitStatusPublication(c.Run.ID, db.CommitStatusPublication{
		SHA: c.Run.HeadSHA, Host: "github.com", Repo: "test/repo", AdoptedOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	pipelineReviewFix(t, c)
	fixed := c.Run.HeadSHA
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(c, types.StepReview, selected, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("post-review fix drift refused: %+v %v", decision, err)
	}
	integrateTargetRewind(t, c, selected)
	data, err := os.ReadFile(filepath.Join(c.WorkDir, "review-fix.txt"))
	if err != nil || string(data) != "review fix survives\n" {
		t.Fatalf("review fix lost: %q %v", data, err)
	}
	if fixed == c.Run.HeadSHA {
		t.Fatal("fix was not replayed onto new target")
	}
	// The fix has not been reviewed: it receives no inherited approval.
	if credit, err := reusableRewindReview(c); err != nil || credit != nil {
		t.Fatalf("unreviewed fix gained credit: %+v %v", credit, err)
	}
}

func TestPostTestTargetAdvanceAfterEarlierRebaseRetainsCleanReview(t *testing.T) {
	c, selected := rewindFixture(t)
	// First integrate a target advance, so HEAD has already been REBASED.
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	integrateTargetRewind(t, c, selected)
	review, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"reviewed_paths":["feature.txt"],"summary":"clean generation 3"}`
	if err := c.DB.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.CompleteReviewStep(review.ID, c.Run.ID, c.Run.HeadSHA, 0, 100, "review.log"); err != nil {
		t.Fatal(err)
	}
	test, err := c.DB.InsertStepResult(c.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := c.DB.InsertStepRound(test.ID, 1, "initial", &findings, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	ids := "[]"
	if err := c.DB.SetStepRoundSelection(round.ID, &ids, db.RoundSelectionSourceUserDeclined); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.SetStepFindings(test.ID, `{"summary":"old test evidence"}`); err != nil {
		t.Fatal(err)
	}
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("REBASED run cannot be REWOUND: %+v %v", decision, err)
	}
	history, err := c.DB.GetTargetRewindHistory(c.Run.ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("missing durable history: %+v %v", history, err)
	}
	latest := history[1]
	if len(latest.Rounds) != 1 || latest.Rounds[0]["selection_source"] != db.RoundSelectionSourceUserDeclined {
		t.Fatalf("gate decision lost: %+v", latest)
	}
	integrateTargetRewind(t, c, selected)
	credit, err := (&ReviewStep{}).Execute(c)
	if err != nil || credit == nil || credit.ReviewApprovedHeadSHA != c.Run.HeadSHA || credit.Findings != findings {
		t.Fatalf("clean identical review not retained: %+v %v", credit, err)
	}
	rows, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepTest && row.Status != types.StepStatusPending {
			t.Fatalf("old Test became new-head evidence: %+v", row)
		}
	}
	// A later local edit, even an empty commit, cannot use the archived credit.
	gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "later change")
	c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if credit, err := reusableRewindReview(c); err != nil || credit != nil {
		t.Fatalf("stale review reused: %+v %v", credit, err)
	}
}

func TestTargetAdvanceReviewFixConflictParksAndPreservesHead(t *testing.T) {
	c, selected := rewindFixture(t)
	pipelineReviewFix(t, c)
	fixed := c.Run.HeadSHA
	gitCmd(t, c.WorkDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(c.WorkDir, "review-fix.txt"), []byte("different target content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "review-fix.txt")
	gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target")
	gitCmd(t, c.WorkDir, "push", "origin", "main")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepReview, selected, false); err != nil {
		t.Fatal(err)
	}
	c.PRContextAfterStep = false
	out, err := (&RebaseStep{}).Execute(c)
	if err != nil || out == nil || !out.NeedsApproval || !strings.Contains(out.Findings, "conflict on rebase") {
		t.Fatalf("conflict did not park clearly: %+v %v", out, err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != fixed {
		t.Fatalf("conflict lost fixes: %s want %s", got, fixed)
	}
	if credit, err := reusableRewindReview(c); err != nil || credit != nil {
		t.Fatalf("conflict gained credit: %+v %v", credit, err)
	}
}

func TestQualifiedPublishedHeadTargetAdvanceDoesNotMoveHead(t *testing.T) {
	for _, kind := range []string{"strict false", "accepted test gap", "accepted test gap after transient failure", "strict true", "conflict", "unknown", "unreadable protection", "ruleset strict", "unqualified"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := rewindFixture(t)
			remote := c.Repo.UpstreamURL
			forgeURL := "https://github.com/test/repo"
			gitCmd(t, c.WorkDir, "config", "url."+remote+".insteadOf", forgeURL)
			c.Repo.UpstreamURL, c.Repo.URLsVerified = forgeURL, true
			c.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
			head := c.Run.HeadSHA
			selection := pipeline.PRTargetSelection{TargetBranch: "main", PRURL: forgeURL + "/pull/42", SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head}
			if _, err := guardPRContextWithSelection(c, types.StepRebase, selection, false); err != nil {
				t.Fatal(err)
			}
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				step, err := c.DB.InsertStepResult(c.Run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.DB.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, head)); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(kind, "accepted test gap") && name == types.StepTest {
					if err := c.DB.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
						t.Fatal(err)
					}
					if err := c.DB.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q,"verdict":"inconclusive"}`, head)); err != nil {
						t.Fatal(err)
					}
					if err := c.DB.SetTestApprovalReason(step.ID, "accepted gap with external coverage"); err != nil {
						t.Fatal(err)
					}
				}
				if name == types.StepReview {
					err = c.DB.CompleteReviewStep(step.ID, c.Run.ID, head, 0, 1, "review.log")
				} else {
					err = c.DB.CompleteStep(step.ID, 0, 1, "step.log")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(kind, "accepted test gap") && localValidationPassed(c, head) {
				t.Fatal("fixture must contain a recorded Test exception")
			}
			if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: head}); err != nil {
				t.Fatal(err)
			}
			if kind != "unqualified" {
				if err := c.DB.SetRunCIReady(c.Run.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			before, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			advanceRewindTarget(t, c)
			protection := `{"strict":false}`
			rules := `[]`
			mergeable := "MERGEABLE"
			switch kind {
			case "strict true":
				protection = `{"strict":true}`
			case "unreadable protection":
				protection = `{}`
			case "ruleset strict":
				rules = `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]`
			case "conflict":
				mergeable = "CONFLICTING"
			case "unknown":
				mergeable = "UNKNOWN"
			}
			bin := fakeCLIBinDir(t)
			linkTestBinary(t, bin, "gh")
			c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_PROTECTION_JSON": protection, "FAKE_CLI_RULES_JSON": rules, "FAKE_CLI_MERGEABLE": mergeable})
			if kind == "accepted test gap after transient failure" {
				healthyEnv := c.Env
				c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_PROTECTION_JSON": `{}`, "FAKE_CLI_RULES_JSON": rules, "FAKE_CLI_MERGEABLE": mergeable})
				if _, err := guardPRContextWithSelection(c, types.StepCI, selection, false); err == nil {
					t.Fatal("unreadable protection was accepted")
				}
				clearCIMonitorReady(c)
				stored, err := c.DB.GetRun(c.Run.ID)
				if err != nil || stored.CIReadyAt != nil {
					t.Fatalf("transient failure did not clear readiness: %+v %v", stored, err)
				}
				c.Env = healthyEnv
			}
			decision, err := guardPRContextWithSelection(c, types.StepCI, selection, false)
			if kind == "strict false" || strings.HasPrefix(kind, "accepted test gap") {
				if err != nil || decision.RestartFrom != "" {
					t.Fatalf("qualified head revalidated: %+v %v", decision, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe policy accepted: %+v", decision)
			}
			after, e := c.DB.GetRunPRContext(c.Run.ID)
			if e != nil || *after != *before {
				t.Fatalf("qualification receipt moved: %+v %v", after, e)
			}
			if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("qualified head moved: %s", got)
			}
		})
	}
}

func TestTargetAdvancePostReviewRetainsBoundaryVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		resolved, needsApproval, hasFindings, wantCredit bool
	}{
		{name: "resolved clean", resolved: true, wantCredit: true},
		{name: "legacy raw verdict"},
		{name: "outstanding approval", resolved: true, needsApproval: true},
		{name: "outstanding finding", resolved: true, hasFindings: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, selected := rewindFixture(t)
			head := c.Run.HeadSHA
			// An older completed review must not override a newer unresolved
			// boundary, including a legacy boundary without resolution proof.
			step, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DB.SetStepFindings(step.ID, `{"findings":[],"reviewed_paths":["feature.txt"]}`); err != nil {
				t.Fatal(err)
			}
			if err := c.DB.CompleteReviewStep(step.ID, c.Run.ID, head, 0, 1, "review.log"); err != nil {
				t.Fatal(err)
			}
			advanceRewindTarget(t, c)
			c.PRContextAfterStep = true
			if _, err := guardPRContextWithSelection(c, types.StepReview, selected, false); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[],"reviewed_paths":["feature.txt"]}`
			if tc.hasFindings {
				findings = `{"findings":[{"id":"b","severity":"warning","file":"feature.txt","description":"operator decision remains open","action":"ask-user"}]}`
			}
			outcome, err := json.Marshal(pipeline.StepOutcome{Findings: findings, ReviewCarryResolved: tc.resolved, NeedsApproval: tc.needsApproval, ReviewApprovedHeadSHA: head, ReviewedPaths: []string{"feature.txt"}, ReviewablePaths: []string{"feature.txt"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DB.RecordTargetRewindBoundary(c.Run.ID, 1, "review", string(outcome)); err != nil {
				t.Fatal(err)
			}
			integrateTargetRewind(t, c, selected)
			credit, err := reusableRewindReview(c)
			if err != nil || (credit != nil) != tc.wantCredit {
				t.Fatalf("credit=%+v want=%t error=%v", credit, tc.wantCredit, err)
			}
			if credit != nil && credit.ReviewApprovedHeadSHA != c.Run.HeadSHA {
				t.Fatalf("credit at wrong head: %+v", credit)
			}
		})
	}
}

func TestTargetAdvanceRetainsHumanDecisionsInPrompts(t *testing.T) {
	c, selected := rewindFixture(t)
	step, err := c.DB.InsertStepResult(c.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	finding := `{"findings":[{"id":"declined","severity":"warning","file":"feature.txt","description":"remove the requested feature"}]}`
	round, err := c.DB.InsertStepRound(step.ID, 1, "initial", &finding, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	ids := "[]"
	if err := c.DB.SetStepRoundSelection(round.ID, &ids, db.RoundSelectionSourceUserDeclined); err != nil {
		t.Fatal(err)
	}
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	prompt := roundHistoryPromptSection(c)
	if !strings.Contains(prompt, "user_chose_to_ignore") || !strings.Contains(prompt, "remove the requested feature") {
		t.Fatalf("gate answer disappeared from prompt: %s", prompt)
	}
}

func TestTargetAdvanceInitiallyUnattachedExistingPR(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	remote := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, dir, "clone", "--bare", dir, remote)
	gitCmd(t, dir, "remote", "set-url", "origin", remote)
	forgeURL := "https://github.com/test/repo"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", forgeURL)
	c.Repo.UpstreamURL, c.Repo.URLsVerified = forgeURL, true
	c.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	c.Config.Rebase.MaxTargetAdvanceRewinds = 3
	pipelineReviewFix(t, c)
	facts := fmt.Sprintf(`{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, head)
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "gh")
	c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_PR_FACTS_LIST_JSON": "[[" + facts + "]]"})
	selection, err := ResolvePRTarget(c)
	if err != nil {
		t.Fatal(err)
	}
	if selection.PRURL != "" || selection.ObservedPRURL == "" {
		t.Fatalf("fixture must discover without attaching: %+v", selection)
	}
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selection, false); err != nil {
		t.Fatal(err)
	}
	observation, err := c.DB.GetTargetRewindPRObservation(c.Run.ID)
	if err != nil || observation == nil || observation.HeadSHA != head {
		t.Fatalf("initial discovery not pinned: %+v %v", observation, err)
	}
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	decision, err := guardPRContextWithSelection(c, types.StepTest, selection, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("stable existing PR blocked rewind: %+v %v", decision, err)
	}
	changed := selection
	changed.ObservedPRURL = "https://github.com/test/repo/pull/43"
	if _, err := guardPRContextWithSelection(c, types.StepRebase, changed, false); err == nil {
		t.Fatal("changed discovery replaced pending identity")
	}
	integrateTargetRewind(t, c, selection)
	durable, err := c.DB.GetRun(c.Run.ID)
	if err != nil || durable.PRURL != nil {
		t.Fatalf("observation granted PR mutation authority: %+v %v", durable, err)
	}
	if content, err := os.ReadFile(filepath.Join(c.WorkDir, "review-fix.txt")); err != nil || string(content) != "review fix survives\n" {
		t.Fatalf("unpublished fix lost: %q %v", content, err)
	}
}
