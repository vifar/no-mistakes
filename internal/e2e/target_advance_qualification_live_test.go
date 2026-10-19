//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the published Git tree (or the exact restored entry head),
// independently of the agent's canned verdict. Only the agent and forge are
// simulated; the CLI, daemon, integration, approval, and publication are real.
func TestTargetAdvanceDuringReviewFixLiveJourney(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conflict=%t", strategy, conflict), func(t *testing.T) {
				scenario := cleanReviewScenario(t)
				h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
				commitTrustedRepoConfig(t, h, "rebase:\n  strategy: "+strategy+"\nauto_fix:\n  rebase: 0\n")
				base := h.UpstreamBranchSHA("main")
				path := "landed.txt"
				if conflict {
					path = "review-fix.txt"
				}
				target := h.CommitChange("main", path, "landed target\n", "prepare target")
				gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
				gitIn(t, h, h.WorkDir, "reset", "--hard", base)
				const branch = "feature/review-fix-advance"
				submitted := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
				old, err := os.ReadFile(scenario)
				if err != nil {
					t.Fatal(err)
				}
				actions := fmt.Sprintf(`actions:
  - match: "Fix-round provenance:"
    structured:
      findings: []
      summary: "fix verified"
      risk_level: low
      risk_rationale: "fixture fix is complete"
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: review-fix.txt
        new: "review fix survives\n"
    git:
      - ["--git-dir=%s", update-ref, refs/heads/main, %s]
    structured:
      findings: []
      summary: "fixed"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: fix-feature
          severity: warning
          file: feature.txt
          line: 1
          description: "Add the missing review fix"
          action: ask-user
      summary: "needs fix"
      risk_level: medium
      risk_rationale: "fixture needs a fix"
      risk_scope: source-or-external
`, h.UpstreamDir, target)
				if err := os.WriteFile(scenario, []byte(strings.Replace(string(old), "actions:\n", actions, 1)), 0600); err != nil {
					t.Fatal(err)
				}
				initFromOwnWorktree(t, h, "init-review-fix")
				h.Checkout(branch)
				h.PushToGate(branch)
				parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
				h.RespondWithFindings(parked.ID, types.StepReview, types.ActionFix, []string{"fix-feature"})
				if conflict {
					run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
					worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
					entry := gitIn(t, h, worktree, "rev-parse", "HEAD")
					if entry != run.HeadSHA || gitIn(t, h, worktree, "show", "HEAD:review-fix.txt") != "review fix survives" {
						t.Fatal("conflict discarded the unpublished review fix")
					}
					if parent := gitIn(t, h, worktree, "rev-parse", "HEAD^"); parent != submitted {
						t.Fatalf("conflict did not restore the review fix on its original submitted parent: %s != %s", parent, submitted)
					}
					if gitIn(t, h, worktree, "status", "--porcelain") != "" {
						t.Fatal("conflict did not restore a clean entry head")
					}
					out, err := h.Run("axi", "status", "--run", run.ID)
					if err != nil || !strings.Contains(strings.ToLower(out), "conflict") {
						t.Fatalf("missing conflict diagnostic: %v\n%s", err, out)
					}
					t.Logf("conflict preserved review fix at %s:\n%s", entry, out)
					h.Respond(run.ID, types.StepRebase, types.ActionAbort)
					if terminal := h.WaitForRun(branch, 30*time.Second); terminal.Status != types.RunFailed {
						t.Fatal("abort did not terminate conflicting integration")
					}
					return
				}
				run := h.WaitForRun(branch, 120*time.Second)
				if run.Status != types.RunCompleted {
					t.Fatalf("review fix target advance refused: %s %s", run.Status, deref(run.Error))
				}
				database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				history, err := database.GetTargetRewindHistory(run.ID)
				_ = database.Close()
				if err != nil || len(history) != 1 {
					t.Fatalf("missing superseded review history: %+v %v", history, err)
				}
				selectionPreserved := false
				for _, round := range history[0].Rounds {
					if round["selection_source"] != db.RoundSelectionSourceUser {
						continue
					}
					raw, _ := round["selected_finding_ids"].(string)
					var ids []string
					if json.Unmarshal([]byte(raw), &ids) == nil && len(ids) == 1 && ids[0] == "fix-feature" {
						selectionPreserved = true
					}
				}
				if !selectionPreserved {
					t.Fatal("target rewind discarded the operator's review-fix selection")
				}
				t.Log("superseded review round retains the operator's selection of fix-feature")
				published := h.UpstreamBranchSHA(branch)
				if !isAncestorIn(t, h, h.UpstreamDir, target, published) || published != run.HeadSHA {
					t.Fatal("publication does not contain verified target")
				}
				for path, want := range map[string]string{"review-fix.txt": "review fix survives", "feature.txt": "submitted feature", "landed.txt": "landed target"} {
					if got := gitIn(t, h, h.UpstreamDir, "show", published+":"+path); got != want {
						t.Fatalf("published %s = %q, want %q", path, got, want)
					}
				}
				logTargetRewindStatus(t, h, run.ID)
				t.Logf("published=%s target=%s; submitted feature and review fix survived %s integration", published, target, strategy)
			})
		}
	}
}

// Exercise initial observation through an operator-configured self-hosted
// forge. Public HTTP/HTTPS URLs are valid; userinfo, hostless URLs, and other
// schemes must fail without pinning an observation or publishing local work.
func TestTargetAdvanceInitialObservationURLLiveJourney(t *testing.T) {
	const host = "forge.example.test"
	const remote = "http://" + host + "/team/repo.git"
	for _, tc := range []struct {
		name, url string
		valid     bool
	}{
		{"http", "http://" + host + "/team/repo/pulls/42", true},
		{"https", "https://" + host + "/team/repo/pulls/42", true},
		{"invalid scheme", "ssh://" + host + "/team/repo/pulls/42", false},
		{"empty host", "http:///team/repo/pulls/42", false},
		{"userinfo", "http://operator@" + host + "/team/repo/pulls/42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{
				Agent: "claude", Scenario: cleanReviewScenario(t),
				GlobalConfigExtra: "provider_plugins:\n  local:\n    command: " + fakeplugin.ExecutableName + "\n    hosts: [" + host + "]\n    timeout: 30s\n",
			})
			configureGitURLRewrite(t, h, remote, h.UpstreamDir)
			gitIn(t, h, h.WorkDir, "remote", "set-url", "origin", remote)
			commitTrustedRepoConfig(t, h, "commands:\n  test: 'false'\nauto_fix:\n  test: 0\n")
			const branch = "feature/initial-http"
			source := h.CommitChange(branch, "feature.txt", "published feature\n", "publish source")
			gitIn(t, h, h.WorkDir, "push", "origin", branch)
			head := h.CommitChange(branch, "unpublished.txt", "local work\n", "extend local head")
			state := filepath.Join(filepath.Dir(h.AgentLog), "plugin-state.json")
			if err := fakeplugin.Save(state, fakeplugin.State{PRs: []fakeplugin.PR{{Number: "42", URL: tc.url, HeadBranch: branch, BaseBranch: "main", HeadSHA: source, State: "open", Title: "existing PR"}}}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(fakeplugin.EnvState, state)
			initFromOwnWorktree(t, h, "init-http-observation")
			h.Checkout(branch)
			h.PushToGate(branch)
			if tc.valid {
				run := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
				database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				observation, err := database.GetTargetRewindPRObservation(run.ID)
				_ = database.Close()
				if err != nil || observation == nil || observation.PRURL != tc.url || observation.HeadSHA != source || run.PRURL != nil {
					t.Fatalf("initial discovery was not pinned without attachment: %+v %v", observation, err)
				}
				t.Logf("accepted %s; pinned PR head=%s while unpublished local head=%s remained unattached", tc.url, source, head)
				h.Respond(run.ID, types.StepTest, types.ActionAbort)
				if terminal := h.WaitForRun(branch, 30*time.Second); terminal.Status != types.RunFailed {
					t.Fatal("abort did not terminate")
				}
			} else {
				run := h.WaitForRun(branch, 90*time.Second)
				if run.Status != types.RunFailed || run.Error == nil {
					t.Fatalf("invalid PR URL was accepted: %s %s", run.Status, deref(run.Error))
				}
				t.Logf("refused %s: %s", tc.name, *run.Error)
			}
			if h.UpstreamBranchSHA(branch) != source {
				t.Fatal("observation published local work")
			}
		})
	}
}

// An accepted Test gap remains a gap. A transient protection failure clears
// readiness, and only fresh permissive protection plus mergeability restores
// it. The exact original receipt, review binding, Test output, and remote head
// are the independent persisted contracts checked across every poll.
func acceptedTestGapScenario(t *testing.T) string {
	t.Helper()
	scenario := cleanReviewScenario(t)
	old, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	action := `actions:
  - match: "You are validating a code change by driving the product itself."
    structured:
      findings: []
      summary: "fixture has an explicit Test gap"
      tested: ["fixture input"]
      testing_summary: "fixture gap"
      artifacts: []
      verdict: inconclusive
      scenarios:
        - {name: "fixture gap", result: untested, live: false, evidence: "fixture input", reason: "deliberate test input"}
`
	if err := os.WriteFile(scenario, []byte(strings.Replace(string(old), "actions:\n", action, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	return scenario
}

func TestTargetAdvanceAcceptedGapRecoversLiveJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: acceptedTestGapScenario(t)})
	j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
	healthy := forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}}
	j.setForge(healthy)
	j.init()
	const branch = "feature/qualified-gap"
	j.push(branch)
	parked := waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
	// Only this disposable run receives the synthetic operator exception.
	client, err := ipc.Dial(paths.WithRoot(j.h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	testGate, _ := findStep(parked.Steps, types.StepTest)
	var response ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: parked.ID, Step: types.StepTest, Action: types.ActionApprove, ApprovalReason: "accept this fixture gap with external coverage", ExpectedGate: testGate.Gate}, &response)
	_ = client.Close()
	if err != nil || !response.OK {
		t.Fatalf("accept fixture gap: %+v %v", response, err)
	}
	run := j.expectReady(branch, 90*time.Second)
	database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	original, err := database.GetRunPRContext(run.ID)
	if err != nil || original == nil || !original.CIQualified {
		t.Fatalf("qualification not recorded: %+v %v", original, err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var testBefore *db.StepResult
	for i := range steps {
		if steps[i].StepName == types.StepTest {
			testBefore = steps[i]
		}
	}
	if testBefore == nil || testBefore.ApprovalReason == nil || testBefore.FindingsJSON == nil || !strings.Contains(*testBefore.FindingsJSON, "inconclusive") {
		t.Fatalf("fixture lost its Test exception: %+v", testBefore)
	}
	head := j.h.UpstreamBranchSHA(branch)
	bodyBefore := livePRBody(t, j.statePath, branch)
	statusWritesBefore := j.ghCalls("/statuses/")
	beforeReads := j.protectionReads()
	target := advanceMain(t, j.h, "new-target.md", "new target\n", "advance qualified target")
	broken := healthy
	broken.Protection = &forgeEndpoint{Status: 503, Message: "temporary protection outage"}
	j.setForge(broken)
	waitReady := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(75 * time.Second)
		for time.Now().Before(deadline) {
			info := j.h.RunInfo(parked.ID)
			if info.CIReady == want {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("readiness never became %t:\n%s", want, j.ciLog(parked.ID))
	}
	assertBound := func() {
		t.Helper()
		current, err := database.GetRunPRContext(run.ID)
		if err != nil || !reflect.DeepEqual(current, original) {
			t.Fatalf("original qualification comparison moved: %+v -> %+v (%v)", original, current, err)
		}
		stored, err := database.GetRun(run.ID)
		if err != nil || stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != head || stored.HeadSHA != head || j.h.UpstreamBranchSHA(branch) != head {
			t.Fatalf("qualified head or review binding moved: %+v %v", stored, err)
		}
		after, err := database.GetStepResult(testBefore.ID)
		if err != nil || !reflect.DeepEqual(after, testBefore) {
			t.Fatalf("accepted Test gap changed: %+v %v", after, err)
		}
		if livePRBody(t, j.statePath, branch) != bodyBefore || j.ghCalls("/statuses/") != statusWritesBefore {
			t.Fatal("target advance rewrote the published attestation or status")
		}
	}
	waitReady(false)
	assertBound()
	t.Logf("protection outage cleared readiness while retaining exact comparison at %s (live target %s)", original.TargetSHA, target)
	j.setForge(healthy)
	waitReady(true)
	assertBound()
	if j.protectionReads() <= beforeReads {
		t.Fatal("recovery did not re-read live protection")
	}
	for _, guard := range []string{"classic strict", "ruleset strict", "unknown policy"} {
		blocked := healthy
		switch guard {
		case "classic strict":
			blocked.Protection = &forgeEndpoint{Body: []byte(`{"strict":true,"contexts":[],"checks":[]}`)}
		case "ruleset strict":
			blocked.Rules = &forgeRules{Pages: []json.RawMessage{json.RawMessage(`[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]`)}}
		case "unknown policy":
			blocked.Protection = &forgeEndpoint{Body: []byte(`{}`)}
		}
		j.setForge(blocked)
		waitReady(false)
		assertBound()
		t.Logf("%s withheld readiness without rewriting qualification", guard)
		j.setForge(healthy)
		waitReady(true)
		assertBound()
	}
	out, err := j.h.Run("axi", "status", "--run", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("healthy polls restored readiness with the accepted Test gap intact:\n%s", out)
	// A different live target is not merely an advance of the qualified one.
	gitIn(t, j.h, j.h.UpstreamDir, "update-ref", "refs/heads/other-target", target)
	state := readStatefulGH(t, j.statePath)
	state.PRs[branch].Base = "other-target"
	writeStatefulGH(t, j.statePath, state)
	waitReady(false)
	assertBound()
	t.Log("retarget withheld readiness; original qualification was not transferred")
	state.PRs[branch].Base = "main"
	writeStatefulGH(t, j.statePath, state)
	waitReady(true)
	assertBound()
	changed := j.h.CommitChange(branch, "outside.txt", "external head change\n", "change remote source")
	gitIn(t, j.h, j.h.WorkDir, "push", "origin", branch)
	waitReady(false)
	current, err := database.GetRunPRContext(run.ID)
	if err != nil || !reflect.DeepEqual(current, original) || j.h.UpstreamBranchSHA(branch) != changed {
		t.Fatalf("changed head gained a qualification receipt: %+v %v", current, err)
	}
	t.Logf("external head %s withheld readiness; qualification remains bound only to %s", changed, head)
	j.finish(branch)
}

// A permissive target policy alone is insufficient: the forge must confirm
// mergeability, and strict shipping never receives the published-head
// exception. The oracle is readiness plus the unchanged remote head and
// persisted original comparison, observed through the real CLI and daemon.
func TestTargetAdvancePublishedGuardsLiveJourney(t *testing.T) {
	for _, guard := range []string{"CONFLICTING", "UNKNOWN", "strict shipping"} {
		t.Run(guard, func(t *testing.T) {
			strict := guard == "strict shipping"
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
			healthy := forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}}
			j.setForge(healthy)
			j.init()
			const branch = "feature/published-guards"
			var run *ipc.RunInfo
			if strict {
				_, run, _ = j.strictRun(branch)
			} else {
				j.push(branch)
				run = j.expectReady(branch, 90*time.Second)
			}
			if !run.CIReady || (run.Shipping != nil) != strict {
				t.Fatalf("fixture not qualified in requested mode: %+v", run)
			}
			head := j.h.UpstreamBranchSHA(branch)
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			original, err := database.GetRunPRContext(run.ID)
			if err != nil || !strict && original == nil {
				t.Fatalf("missing original comparison: %+v %v", original, err)
			}
			shippingComparison, err := database.GetReviewComparison(run.ID)
			if err != nil || strict && shippingComparison == nil {
				t.Fatalf("missing strict comparison: %+v %v", shippingComparison, err)
			}
			waitReady := func(want bool, timeout time.Duration) {
				t.Helper()
				deadline := time.Now().Add(timeout)
				for time.Now().Before(deadline) {
					if j.h.RunInfo(run.ID).CIReady == want {
						return
					}
					time.Sleep(200 * time.Millisecond)
				}
				t.Fatalf("readiness never became %t:\n%s", want, j.ciLog(run.ID))
			}
			assertBound := func() {
				t.Helper()
				current, err := database.GetRunPRContext(run.ID)
				if err != nil || !reflect.DeepEqual(current, original) || j.h.UpstreamBranchSHA(branch) != head {
					t.Fatalf("guard moved the published head/comparison: %+v -> %+v (%v)", original, current, err)
				}
				currentShipping, err := database.GetReviewComparison(run.ID)
				expectedShipping := shippingComparison
				if strict {
					// Strict shipping invalidates the old approval rather than
					// receiving the ordinary published-head exception.
					expected := *shippingComparison
					expected.Approved = false
					expectedShipping = &expected
				}
				if err != nil || !reflect.DeepEqual(currentShipping, expectedShipping) {
					t.Fatalf("guard transferred strict qualification: %+v -> %+v (%v)", shippingComparison, currentShipping, err)
				}
			}
			if !strict {
				blocked := healthy
				blocked.Mergeable = guard
				j.setForge(blocked)
			}
			advanceMain(t, j.h, "target.txt", "advanced target\n", "advance target after qualification")
			waitReady(false, 360*time.Second)
			assertBound()
			t.Logf("%s withheld readiness after target advance at unchanged published head %s:\n%s", guard, head, j.ciLog(run.ID))
			j.finish(branch)
		})
	}
}
