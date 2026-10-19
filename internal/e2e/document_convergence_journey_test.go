//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// These journeys use the #65/#90 acceptance contracts as their oracle. The
// CLI, daemon, Git, accounting, and publication renderer are real. Only agent
// responses and the GitHub service are simulated. Recovery fixtures explicitly
// seed persisted state while the owned daemon is stopped; they do not call a
// Document helper as a substitute for executing the product.
func documentJourneyScenario(t *testing.T, path string, action map[string]any) string {
	t.Helper()
	base, err := os.ReadFile(cleanReviewScenario(t))
	if err != nil {
		t.Fatal(err)
	}
	var scenario struct {
		Actions []map[string]any `yaml:"actions" json:"actions"`
	}
	if err := yaml.Unmarshal(base, &scenario); err != nil {
		t.Fatal(err)
	}
	if action != nil {
		action["match"] = "Find what this change made stale"
		scenario.Actions = append([]map[string]any{action}, scenario.Actions...)
	}
	if path == "" {
		path = filepath.Join(t.TempDir(), "document-journey.json")
	}
	raw, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func documentJourneyAction(path, content string, gaps ...string) map[string]any {
	findings := []map[string]any{}
	for i, gap := range gaps {
		findings = append(findings, map[string]any{
			"id": fmt.Sprintf("gap-%d", i+1), "severity": "warning", "action": "ask-user",
			"category": "documentation", "file": "README.md", "description": gap,
		})
	}
	action := map[string]any{"structured": map[string]any{"findings": findings, "summary": "repair documentation"}}
	if path != "" {
		action["edits"] = []map[string]string{{"path": path, "new": content}}
	}
	return action
}

func documentJourneyDB(t *testing.T, h *Harness, use func(*db.DB)) {
	t.Helper()
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	use(database)
}

func documentJourneyPasses(t *testing.T, h *Harness, runID string) []db.DocumentPass {
	t.Helper()
	var passes []db.DocumentPass
	documentJourneyDB(t, h, func(database *db.DB) {
		var err error
		passes, err = database.GetDocumentPasses(runID)
		if err != nil {
			t.Fatal(err)
		}
	})
	gateJourneyEvidence(t, "persisted Document passes", passes)
	return passes
}

func documentJourneyStatus(t *testing.T, h *Harness, runID string) {
	t.Helper()
	out, err := h.Run("axi", "status", "--run", runID)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	t.Logf("actual CLI status:\n%s", out)
}

func documentJourneyNextGate(t *testing.T, h *Harness, runID string, stepName types.StepName, previous string) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run := h.RunInfo(runID)
		step, ok := findStep(run.Steps, stepName)
		if ok && step.Gate != nil && step.Gate.Nonce != previous &&
			(step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview) {
			return run
		}
		if run.Status.Terminal() {
			h.dumpDebugState()
			t.Fatalf("run ended before %s gate: %s %s; steps=%+v", stepName, run.Status, deref(run.Error), run.Steps)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no fresh %s gate for run %s", stepName, runID)
	return nil
}

func documentJourneyFix(t *testing.T, h *Harness, run *ipc.RunInfo) string {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepDocument)
	if !ok || step.FindingsJSON == nil || step.Gate == nil {
		t.Fatalf("missing Document gate: %+v", run.Steps)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, finding := range findings.Items {
		ids = append(ids, finding.ID)
	}
	h.RespondWithFindings(run.ID, types.StepDocument, types.ActionFix, ids)
	return step.Gate.Nonce
}

func documentJourneyStart(t *testing.T, h *Harness, extra string) (string, string) {
	t.Helper()
	commitTrustedRepoConfig(t, h, extra)
	initFromOwnWorktree(t, h, "init-document-journey")
	const branch = "feature/document-journey"
	head := h.CommitChange(branch, "feature.txt", "BAD spacing\n", "submit feature")
	h.PushToGate(branch)
	return branch, head
}

func TestDocumentJourneyOverBudgetDiscardSurvivesRecovery(t *testing.T) {
	scenario := documentJourneyScenario(t, "", documentJourneyAction("README.md", "accepted guide\n", "first gap", "second gap"))
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	branch, submitted := documentJourneyStart(t, h, "commands:\n  lint: 'true'\n")
	first := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
	accepted := first.HeadSHA
	if accepted == submitted {
		t.Fatal("initial README repair was not committed")
	}
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(first.RepoID, first.ID)
	proposal := documentJourneyAction("guide-a.md", "proposal A\n")
	proposal["edits"] = []map[string]string{{"path": "guide-a.md", "new": "proposal A\n"}, {"path": "guide-b.md", "new": "proposal B\n"}}
	proposal["git"] = [][]string{{"add", "guide-a.md"}, {"commit", "-m", "first candidate"}, {"add", "guide-b.md"}, {"commit", "-m", "second candidate"}}
	documentJourneyScenario(t, scenario, proposal)
	for attempt := 0; attempt < 2; attempt++ {
		previous := documentJourneyFix(t, h, first)
		first = documentJourneyNextGate(t, h, first.ID, types.StepDocument, previous)
		if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != accepted || first.HeadSHA != accepted {
			t.Fatalf("rollback did not restore accepted head: actual=%s recorded=%s want=%s", got, first.HeadSHA, accepted)
		}
		if status := gitIn(t, h, worktree, "status", "--porcelain"); status != "" {
			t.Fatalf("candidate files survived rollback: %s", status)
		}
		gateJourneyDaemon(t, h, "stop")
		passes := documentJourneyPasses(t, h, first.ID)
		if len(passes) != attempt+2 {
			t.Fatalf("passes=%+v", passes)
		}
		commits := 0
		for _, pass := range passes {
			commits += pass.CommitCount
		}
		last := passes[len(passes)-1]
		if commits != 1 || last.CommitCount != 0 || last.ToHeadSHA == nil || *last.ToHeadSHA != accepted || last.FindingsJSON == nil {
			t.Fatalf("discard consumed budget or was not persisted: %+v", passes)
		}
		findings, err := types.ParseFindingsJSON(*last.FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		gaps, discards := map[string]int{}, 0
		for _, finding := range findings.Items {
			gaps[finding.Description]++
			if finding.Action == "ask-user" && strings.Contains(finding.Description, "discarded an over-budget proposal") {
				discards++
			}
		}
		if gaps["first gap"] != 1 || gaps["second gap"] != 1 || discards != 1 || len(findings.Items) != 3 {
			t.Fatalf("want retained gaps and one durable discard: %s", *last.FindingsJSON)
		}
		gateJourneyDaemon(t, h, "start")
		first = documentJourneyNextGate(t, h, first.ID, types.StepDocument, "")
		documentJourneyStatus(t, h, first.ID)
	}
}

func TestDocumentJourneySameFileContentContext(t *testing.T) {
	for _, kind := range []string{"empty", "identical-tree merge", "changed"} {
		t.Run(kind, func(t *testing.T) {
			changed := kind == "changed"
			scenario := documentJourneyScenario(t, "", documentJourneyAction("README.md", "accepted guide\n", "retained gap"))
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			branch, _ := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 2\ncommands:\n  lint: 'true'\n")
			first := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
			accepted := first.HeadSHA
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(first.RepoID, first.ID)
			// A recovered run can have an intervening pipeline-authored head.
			// Seed that boundary while stopped, then let the real daemon decide
			// whether another edit of the SAME README is a repeated repair.
			gateJourneyDaemon(t, h, "stop")
			if changed {
				if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("changed inputs\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, h, worktree, "add", "feature.txt")
			}
			gitIn(t, h, worktree, "commit", "--allow-empty", "-m", "intervening pipeline input")
			if kind == "identical-tree merge" {
				parent := gitIn(t, h, worktree, "rev-parse", "HEAD")
				merged := gitIn(t, h, worktree, "commit-tree", "HEAD^{tree}", "-p", accepted, "-p", parent, "-m", "identical-tree two-parent merge")
				gitIn(t, h, worktree, "update-ref", "HEAD", merged, parent)
				if parents := parentsOf(t, h, worktree, "HEAD"); len(parents) != 2 {
					t.Fatalf("fixture is not a two-parent merge: %v", parents)
				}
			}
			entry := gitIn(t, h, worktree, "rev-parse", "HEAD")
			documentJourneyDB(t, h, func(database *db.DB) {
				if err := database.UpdateRunHeadSHA(first.ID, entry); err != nil {
					t.Fatal(err)
				}
			})
			documentJourneyScenario(t, scenario, documentJourneyAction("README.md", "second guide\n", "retained gap"))
			gateJourneyDaemon(t, h, "start")
			first = documentJourneyNextGate(t, h, first.ID, types.StepDocument, "")
			previous := documentJourneyFix(t, h, first)
			second := documentJourneyNextGate(t, h, first.ID, types.StepDocument, previous)
			passes := documentJourneyPasses(t, h, first.ID)
			if len(passes) != 2 || passes[1].ToHeadSHA == nil {
				t.Fatalf("missing completed second pass: %+v", passes)
			}
			wantText, wantCommits := "accepted guide", 0
			if changed {
				wantText, wantCommits = "second guide", 1
			}
			if passes[1].CommitCount != wantCommits || gitIn(t, h, worktree, "show", "HEAD:README.md") != wantText {
				t.Fatalf("same-file content decision incorrect: %+v", passes)
			}
			if !changed && second.HeadSHA != entry {
				t.Fatalf("identical-tree proposal moved head: %s want=%s (accepted=%s)", second.HeadSHA, entry, accepted)
			}
			documentJourneyStatus(t, h, first.ID)
		})
	}
}

const documentJourneyBoundary = "gates:\n  - name: boundary\n    after: test\n    command: 'false'\n"

func documentJourneyFailed(t *testing.T, h *Harness, branch, cause string) *ipc.RunInfo {
	t.Helper()
	run := h.WaitForRun(branch, 90*time.Second)
	step, ok := findStep(run.Steps, types.StepDocument)
	if run.Status != types.RunFailed || !ok || step.Status != types.StepStatusFailed || step.Error == nil || !strings.Contains(*step.Error, cause) {
		t.Fatalf("want Document refusal %q: run=%s error=%s step=%+v", cause, run.Status, deref(run.Error), step)
	}
	documentJourneyStatus(t, h, run.ID)
	// Terminal status precedes deferred worktree cleanup. Join the owned
	// daemon before inspecting preservation, then exercise startup cleanup
	// too: a successful early read must not race ahead of later deletion.
	gateJourneyDaemon(t, h, "stop")
	gateJourneyDaemon(t, h, "start")
	gateJourneyDaemon(t, h, "stop")
	return run
}

func documentJourneyCalls(h *Harness) int {
	count := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Find what this change made stale") {
			count++
		}
	}
	return count
}

func TestDocumentJourneyAdmissionAndInterruptedHistory(t *testing.T) {
	for _, kind := range []string{"dirty", "interrupted", "assume-unchanged", "skip-worktree", "flag-without-edit"} {
		t.Run(kind, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", documentJourneyAction("README.md", "must not apply\n"))})
			branch, submitted := documentJourneyStart(t, h, documentJourneyBoundary+"commands:\n  lint: 'true'\ndocument:\n  max_commits_per_run: 0\n")
			boundary := types.CustomGateStepName(types.StepTest, "boundary")
			parked := waitForStepStatus(t, h, branch, boundary, types.StepStatusAwaitingApproval, 90*time.Second)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
			cause := "clean worktree"
			wantPasses := 0
			if kind == "interrupted" {
				gateJourneyDaemon(t, h, "stop")
				documentJourneyDB(t, h, func(database *db.DB) {
					if _, err := database.BeginDocumentPass(parked.ID, submitted); err != nil {
						t.Fatal(err)
					}
				})
				gateJourneyDaemon(t, h, "start")
				parked = documentJourneyNextGate(t, h, parked.ID, boundary, "")
				cause, wantPasses = "was interrupted", 1
			} else {
				if kind != "dirty" {
					flag := "--assume-unchanged"
					if kind == "skip-worktree" {
						flag = "--skip-worktree"
					}
					gitIn(t, h, worktree, "update-index", flag, "feature.txt")
					cause = "1 entries are hidden from status by index flags"
				}
				if kind != "flag-without-edit" {
					if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("owner's uncommitted bytes\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
			if err != nil {
				t.Fatal(err)
			}
			flags := gitIn(t, h, worktree, "ls-files", "-v")
			h.Respond(parked.ID, boundary, types.ActionApprove)
			failed := documentJourneyFailed(t, h, branch, cause)
			after, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
			if err != nil || string(after) != string(before) || gitIn(t, h, worktree, "rev-parse", "HEAD") != submitted || gitIn(t, h, worktree, "ls-files", "-v") != flags {
				t.Fatalf("refusal mutated owner bytes, head, or flags: before=%q after=%q error=%v", before, after, err)
			}
			passes := documentJourneyPasses(t, h, failed.ID)
			if len(passes) != wantPasses || documentJourneyCalls(h) != 0 || wantPasses == 1 && passes[0].ToHeadSHA != nil {
				t.Fatalf("refusal launched agent or changed pass state: calls=%d passes=%+v", documentJourneyCalls(h), passes)
			}
		})
	}
}

func TestDocumentJourneyMidPassFlagsPreserveProposal(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			action := documentJourneyAction("README.md", "first proposal\n")
			action["edits"] = []map[string]string{{"path": "README.md", "new": "first proposal\n"}, {"path": "guide.md", "new": "second proposal\n"}, {"path": "feature.txt", "new": "hidden owner bytes\n"}}
			// Hide feature.txt after its raw bytes change, then commit only the
			// two documentation files. Exceeding limit 1 requires rollback.
			action["git"] = [][]string{{"update-index", flag, "feature.txt"}, {"add", "README.md"}, {"commit", "-m", "first proposal"}, {"add", "guide.md"}, {"commit", "-m", "second proposal"}}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", action)})
			branch, submitted := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 1\ncommands:\n  lint: 'true'\n")
			failed := documentJourneyFailed(t, h, branch, "1 entries are hidden from status by index flags")
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(failed.RepoID, failed.ID)
			if count := gitIn(t, h, worktree, "rev-list", "--count", submitted+"..HEAD"); count != "2" {
				t.Fatalf("refusal lost proposal commits: %s", count)
			}
			for path, want := range map[string]string{"README.md": "first proposal\n", "guide.md": "second proposal\n", "feature.txt": "hidden owner bytes\n"} {
				got, err := os.ReadFile(filepath.Join(worktree, path))
				if err != nil || string(got) != want {
					t.Fatalf("refusal lost %s: %q %v", path, got, err)
				}
			}
			flags := gitIn(t, h, worktree, "ls-files", "-v")
			wantFlag := "h feature.txt"
			if flag == "--skip-worktree" {
				wantFlag = "S feature.txt"
			}
			if !strings.Contains(flags, wantFlag) {
				t.Fatalf("refusal cleared hidden flag: %s", flags)
			}
			passes := documentJourneyPasses(t, h, failed.ID)
			if len(passes) != 1 || passes[0].ToHeadSHA != nil || passes[0].CommitCount != 0 {
				t.Fatalf("unsafe rollback falsely completed the pass: %+v", passes)
			}
		})
	}
}

func TestDocumentJourneyHiddenIndexPrecedesCachedCompletion(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", documentJourneyAction("README.md", "accepted guide\n"))})
			branch, _ := documentJourneyStart(t, h, "commands:\n  lint: 'false'\n")
			parked := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 90*time.Second)
			accepted := parked.HeadSHA
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
			gitIn(t, h, worktree, "update-index", flag, "feature.txt")
			const hidden = "owner bytes after accepted Document pass\n"
			if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte(hidden), 0600); err != nil {
				t.Fatal(err)
			}
			flags := gitIn(t, h, worktree, "ls-files", "-v")
			documentJourneyReplay(t, h, parked.ID)
			failed := documentJourneyFailed(t, h, branch, "1 entries are hidden from status by index flags")
			got, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
			if err != nil || string(got) != hidden || gitIn(t, h, worktree, "rev-parse", "HEAD") != accepted || gitIn(t, h, worktree, "ls-files", "-v") != flags {
				t.Fatalf("cached-entry refusal changed hidden work: bytes=%q error=%v", got, err)
			}
			passes := documentJourneyPasses(t, h, failed.ID)
			if len(passes) != 1 || documentJourneyCalls(h) != 1 {
				t.Fatalf("cached-entry refusal launched another pass: %+v calls=%d", passes, documentJourneyCalls(h))
			}
		})
	}
}

func TestDocumentJourneyTrustedLimits(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", documentJourneyAction("README.md", "must not apply\n"))})
			commitTrustedRepoConfig(t, h, fmt.Sprintf("document:\n  max_commits_per_run: %d\ncommands:\n  lint: 'true'\n", limit))
			initFromOwnWorktree(t, h, "init-document-limit")
			const branch = "feature/document-limit"
			h.CommitChange(branch, "feature.txt", "feature\n", "feature")
			// A branch may neither repair a trusted negative value nor widen
			// a trusted zero budget. The daemon must read the pinned main.
			submitted := h.CommitChange(branch, ".no-mistakes.yaml", harnessRepoConfigWithExtra("document:\n  max_commits_per_run: 20\ncommands:\n  lint: 'true'\n"), "attempt branch budget widening")
			h.PushToGate(branch)
			if limit < 0 {
				run := h.WaitForRun(branch, 90*time.Second)
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "max_commits_per_run") {
					t.Fatalf("trusted negative limit accepted: %+v", run)
				}
				documentJourneyStatus(t, h, run.ID)
				if documentJourneyCalls(h) != 0 {
					t.Fatal("invalid trusted config launched Document")
				}
				return
			}
			parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
			if parked.HeadSHA != submitted || documentJourneyCalls(h) != 0 || len(documentJourneyPasses(t, h, parked.ID)) != 0 {
				t.Fatal("branch widened trusted zero budget")
			}
			step, _ := findStep(parked.Steps, types.StepDocument)
			if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "commit limit reached") || strings.Contains(*step.FindingsJSON, "discard") {
				t.Fatalf("zero-budget admission falsely reported a discard: %+v", step)
			}
			gateJourneyDaemon(t, h, "stop")
			gateJourneyDaemon(t, h, "start")
			parked = documentJourneyNextGate(t, h, parked.ID, types.StepDocument, "")
			previous := documentJourneyFix(t, h, parked)
			parked = documentJourneyNextGate(t, h, parked.ID, types.StepDocument, previous)
			if documentJourneyCalls(h) != 0 || len(documentJourneyPasses(t, h, parked.ID)) != 0 {
				t.Fatal("recovery lost trusted budget")
			}
			documentJourneyStatus(t, h, parked.ID)
		})
	}
}

// Restore a Document gate after a suffix reset, as an older persisted run may
// contain. The subsequent response, Execute, cache decision, and all later
// steps run in the real daemon. This boundary seed is intentionally disclosed.
func documentJourneyReplay(t *testing.T, h *Harness, runID string) {
	t.Helper()
	gateJourneyDaemon(t, h, "stop")
	documentJourneyDB(t, h, func(database *db.DB) {
		steps, err := database.GetStepsByRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		var document *db.StepResult
		for _, step := range steps {
			if step.StepName == types.StepDocument {
				document = step
			}
		}
		if document == nil || document.FindingsJSON == nil {
			t.Fatal("missing persisted Document result")
		}
		if err := database.ResetStepsFrom(runID, types.StepDocument.Order()); err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(document.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.ParkStepForApproval(runID, document.ID, types.StepStatusAwaitingApproval, 0, 0, document.FindingsJSON); err != nil {
			t.Fatal(err)
		}
		if err := database.SetRunAwaitingAgent(runID); err != nil {
			t.Fatal(err)
		}
	})
	gateJourneyDaemon(t, h, "start")
	parked := documentJourneyNextGate(t, h, runID, types.StepDocument, "")
	documentJourneyFix(t, h, parked)
}

func TestDocumentJourneyCarryStopsAndPublishesHistoricalEvidence(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", documentJourneyAction("README.md", "accepted documentation\n"))})
	state := setupStatefulGitHub(t, h)
	allow := filepath.Join(t.TempDir(), "allow-publication")
	branch, submitted := documentJourneyStart(t, h, fmt.Sprintf("commands:\n  lint: %q\n", "test -f "+shellQuote(allow)))
	parked := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 90*time.Second)
	accepted := parked.HeadSHA
	var generation int64
	var hasComparison bool
	documentJourneyDB(t, h, func(database *db.DB) {
		comparison, err := database.GetReviewComparison(parked.ID)
		if err != nil {
			t.Fatal(err)
		}
		hasComparison = comparison != nil
		if hasComparison {
			generation = comparison.Generation
		}
	})
	assertCredit := func() {
		t.Helper()
		documentJourneyDB(t, h, func(database *db.DB) {
			run, err := database.GetRun(parked.ID)
			if err != nil || run == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != accepted || accepted == submitted {
				t.Fatalf("doc-only Review credit was not carried: %+v %v", run, err)
			}
			comparison, err := database.GetReviewComparison(run.ID)
			if err != nil || (comparison != nil) != hasComparison || comparison != nil && comparison.Generation != generation {
				t.Fatalf("carry changed review generation: %+v %v", comparison, err)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.StepName != types.StepReview && step.StepName != types.StepTest {
					continue
				}
				rounds, err := database.GetRoundsByStep(step.ID)
				if err != nil || len(rounds) != 1 {
					t.Fatalf("carry reran %s: %+v %v", step.StepName, rounds, err)
				}
				if step.StepName == types.StepReview && (rounds[0].ReviewedHeadSHA == nil || *rounds[0].ReviewedHeadSHA != submitted) {
					t.Fatalf("historical Review head was restamped: %+v", rounds)
				}
				if step.StepName == types.StepTest {
					if rounds[0].FindingsJSON == nil || step.FindingsJSON == nil {
						t.Fatal("missing historical Test evidence")
					}
					historical, err := types.ParseFindingsJSON(*rounds[0].FindingsJSON)
					if err != nil || historical.TestedHeadSHA != submitted || historical.Verdict != types.TestVerdictGo {
						t.Fatalf("historical Test evidence was restamped: %+v %v", historical, err)
					}
					current, err := types.ParseFindingsJSON(*step.FindingsJSON)
					if err != nil || current.TestedHeadSHA != accepted {
						t.Fatalf("Test credit did not carry atomically: %+v %v", current, err)
					}
				}
			}
		})
	}
	assertCredit()
	for attempt := 0; attempt < 2; attempt++ {
		documentJourneyReplay(t, h, parked.ID)
		parked = documentJourneyNextGate(t, h, parked.ID, types.StepLint, "")
		passes := documentJourneyPasses(t, h, parked.ID)
		if len(passes) != 1 || passes[0].CommitCount != 1 || passes[0].CarriedReviewFrom == nil || *passes[0].CarriedReviewFrom != submitted || documentJourneyCalls(h) != 1 || parked.HeadSHA != accepted {
			t.Fatalf("repeated clean execution created another proposal: passes=%+v calls=%d head=%s", passes, documentJourneyCalls(h), parked.HeadSHA)
		}
		assertCredit()
	}
	// Release Lint by making its command actually pass, without approval over
	// a failure or a skipped step. The catch-all agent makes no source edits.
	if err := os.WriteFile(allow, []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h.Respond(parked.ID, types.StepLint, types.ActionFix)
	// The real CI monitor remains active on an open PR. This journey ends at
	// actual PR publication; harness cleanup stops its owned daemon afterward.
	run := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 90*time.Second)
	if run.HeadSHA != accepted || h.UpstreamBranchSHA(branch) != accepted {
		t.Fatalf("publication failed: status=%s head=%s error=%s", run.Status, run.HeadSHA, deref(run.Error))
	}
	body := livePRBody(t, state, branch)
	const marker = "<!-- no-mistakes-pipeline-attestation:v1 "
	_, tail, ok := strings.Cut(body, marker)
	if !ok {
		t.Fatalf("published body has no attestation: %s", body)
	}
	payload, _, ok := strings.Cut(tail, " -->")
	if !ok {
		t.Fatal("published attestation is unterminated")
	}
	var attestation struct {
		HeadSHA        string          `json:"head_sha"`
		LiveValidation json.RawMessage `json:"live_validation"`
	}
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
		t.Fatal(err)
	}
	if attestation.HeadSHA != accepted || len(attestation.LiveValidation) != 0 {
		t.Fatalf("published attestation restamped historical live credit: %s", payload)
	}
	if !strings.Contains(body, "## Testing") || !strings.Contains(body, "simulated tests passed") {
		t.Fatalf("publication lost the historical Testing disclosure: %s", body)
	}
	assertCredit()
	documentJourneyStatus(t, h, run.ID)
	t.Logf("actual published PR body (simulated forge):\n%s", body)
}

func TestDocumentJourneyUnsafeAndStrictRestart(t *testing.T) {
	for _, kind := range []string{"code", "configuration", "instructions", "intermediate-code-revert", "executable-document", "trusted-paths", "hidden-gitlink", "strict"} {
		t.Run(kind, func(t *testing.T) {
			path := "README.md"
			switch kind {
			case "code":
				path = "main.go"
			case "configuration":
				path = "example.yaml"
			case "instructions":
				path = "AGENTS.md"
			case "trusted-paths":
				path = "guides/feature.md"
			}
			action := documentJourneyAction(path, "updated\n")
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", action)})
			// A trusted broad path selection still cannot admit executable or
			// instruction changes. Strict runs use the real shipping CLI.
			trustedPaths := "['**']"
			if kind == "trusted-paths" {
				trustedPaths = "['docs/**']"
			}
			commitTrustedRepoConfig(t, h, "commands:\n  lint: 'true'\ndocument:\n  max_commits_per_run: 2\n  doc_only_paths: "+trustedPaths+"\nno_ci: true\n")
			if kind == "intermediate-code-revert" {
				action["edits"] = []map[string]string{{"path": "README.md", "new": "updated\n"}, {"path": "feature.txt", "new": "temporary code change\n"}}
				action["git"] = [][]string{{"add", "feature.txt"}, {"commit", "-m", "intermediate executable change"}, {"restore", "--source=HEAD^", "--staged", "--worktree", "--", "feature.txt"}, {"add", "README.md"}, {"commit", "-m", "restore feature and document"}}
				documentJourneyScenario(t, h.Scenario, action)
			}
			if kind == "executable-document" {
				action["git"] = [][]string{{"config", "core.filemode", "false"}, {"add", "README.md"}, {"update-index", "--chmod=+x", "README.md"}, {"commit", "-m", "executable documentation"}}
				documentJourneyScenario(t, h.Scenario, action)
			}
			if kind == "hidden-gitlink" {
				// A gitlink need not have .gitmodules or a checked-out directory.
				// Hide its diff using local submodule config, as in #65.
				old := h.UpstreamBranchSHA("main")
				new := h.CommitChange("main", "module.txt", "new module\n", "new module commit")
				gitIn(t, h, h.WorkDir, "push", "origin", "main")
				action["git"] = [][]string{{"config", "diff.ignoreSubmodules", "all"}, {"update-index", "--add", "--cacheinfo", "160000," + new + ",module"}, {"commit", "-m", "update hidden gitlink"}}
				// The submitted branch starts with the older gitlink.
				gitIn(t, h, h.WorkDir, "update-index", "--add", "--cacheinfo", "160000,"+old+",module")
				gitIn(t, h, h.WorkDir, "commit", "-m", "seed hidden gitlink")
				gitIn(t, h, h.WorkDir, "push", "origin", "main")
				documentJourneyScenario(t, h.Scenario, action)
			}
			state := setupStatefulGitHub(t, h)
			initFromOwnWorktree(t, h, "init-document-restart")
			const branch = "feature/document-restart"
			submitted := h.CommitChange(branch, "feature.txt", "feature\n", "submit feature")
			if kind == "trusted-paths" {
				submitted = h.CommitChange(branch, ".no-mistakes.yaml", harnessRepoConfigWithExtra("document:\n  doc_only_paths: ['**']\n"), "attempt branch path widening")
			}
			if kind == "strict" {
				wt := h.AddWorktree(branch)
				out, err := h.RunInDir(wt, "axi", "run", "--shipping-mode", "full", "--intent", "verify strict documentation restart", "--wait", "2s")
				// A bounded CLI hold reports elapsed as exit 1 while its daemon
				// continues. Independently await and assert publication below.
				if err != nil && !strings.Contains(out, "wait of 2s elapsed") {
					t.Fatalf("strict run: %v\n%s", err, out)
				}
				t.Logf("strict CLI output:\n%s", out)
			} else {
				h.PushToGate(branch)
			}
			run := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 90*time.Second)
			if kind == "strict" && (run.Shipping == nil || run.Shipping.Mode != types.ShippingFull) {
				t.Fatalf("CLI did not launch strict shipping: %+v", run.Shipping)
			}
			if run.HeadSHA == submitted || h.UpstreamBranchSHA(branch) != run.HeadSHA {
				t.Fatalf("unsafe restart failed: status=%s error=%s", run.Status, deref(run.Error))
			}
			reviews := 0
			for _, invocation := range h.AgentInvocations() {
				if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
					reviews++
				}
			}
			passes := documentJourneyPasses(t, h, run.ID)
			if reviews != 2 || documentJourneyCalls(h) != 1 || len(passes) != 1 || passes[0].CarriedReviewFrom != nil {
				t.Fatalf("unsafe or strict range earned carry: reviews=%d calls=%d passes=%+v", reviews, documentJourneyCalls(h), passes)
			}
			if !strings.Contains(livePRBody(t, state, branch), run.HeadSHA) {
				t.Fatal("published attestation does not bind the revalidated head")
			}
			documentJourneyStatus(t, h, run.ID)
		})
	}
}

func TestDocumentJourneyDiscardKeepsCombinedLintDuty(t *testing.T) {
	scenario := documentJourneyScenario(t, "", documentJourneyAction("README.md", "BAD spacing\n", "pending documentation gap"))
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	branch, _ := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 2\n")
	first := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
	accepted := first.HeadSHA
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(first.RepoID, first.ID)
	documentJourneyScenario(t, scenario, documentJourneyAction("README.md", "GOOD spacing\n"))
	// The fake lint boundary refuses to answer unless Git confirms that the
	// running product restored the accepted bytes. It then reports the
	// unresolved formatter finding; no real formatter is invoked by this test.
	raw, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Actions []map[string]any `json:"actions"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	lint := map[string]any{
		"match": "Detect the linting and formatting tools for this project",
		"git":   [][]string{{"diff", "--exit-code", accepted, "--", "README.md"}},
		"structured": map[string]any{
			"findings": []map[string]any{{"id": "restored-formatting", "severity": "warning", "file": "README.md", "action": "ask-user", "description": "BAD spacing remains after the discarded repair"}},
			"summary":  "formatting still needs attention",
		},
	}
	fixture.Actions = append([]map[string]any{lint}, fixture.Actions...)
	raw, err = json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scenario, raw, 0600); err != nil {
		t.Fatal(err)
	}
	previous := documentJourneyFix(t, h, first)
	discarded := documentJourneyNextGate(t, h, first.ID, types.StepDocument, previous)
	if discarded.HeadSHA != accepted || gitIn(t, h, worktree, "show", "HEAD:README.md") != "BAD spacing" {
		t.Fatal("discard failed to restore the accepted BAD spacing")
	}
	step, _ := findStep(discarded.Steps, types.StepDocument)
	if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "discarded a repeated repair") || !strings.Contains(*step.FindingsJSON, "pending documentation gap") {
		t.Fatalf("discard silently approved the pending gap: %+v", step)
	}
	// An explicit fixture operator acknowledges the documentation warning so
	// the next step can demonstrate that discarded housekeeping credit is gone.
	h.Respond(discarded.ID, types.StepDocument, types.ActionApprove)
	parked := documentJourneyNextGate(t, h, discarded.ID, types.StepLint, "")
	step, _ = findStep(parked.Steps, types.StepLint)
	if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "BAD spacing remains") {
		t.Fatalf("discarded clean lint credit suppressed reassessment: %+v", step)
	}
	lintCalls := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Detect the linting and formatting tools for this project") {
			lintCalls++
		}
	}
	if lintCalls != 1 || documentJourneyCalls(h) != 2 {
		t.Fatalf("combined duty was lost: Document=%d Lint=%d", documentJourneyCalls(h), lintCalls)
	}
	documentJourneyStatus(t, h, parked.ID)
}
