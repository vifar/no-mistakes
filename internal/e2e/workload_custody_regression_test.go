//go:build e2e && !windows

package e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A persisted row with no workload owned by this daemon must neither block
// a planned stop nor be claimed and rewritten as though it were cancelled.
func TestPlannedStopIgnoresAnUnownedRunOnRealDaemon(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.InsertRun(h.repoID(), "unowned", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	out, err := h.Run("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("unowned row blocked planned stop: %v\n%s", err, out)
	}
	assertNoDaemonProcessesForRoot(t, h, "after stopping with an unowned row")
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("shutdown changed an unowned row: %+v %v", stored, err)
	}
	t.Logf("RESULT stop output: %s; unowned row remains %s and daemon process exited", strings.TrimSpace(out), stored.Status)
}

// The workload/custody contract requires a second workload to finish while
// the first run's outcome cannot settle. The oracle is the second run's
// durable completion and the first run's retained worktree, not a slot count.
func TestStorageBlockedFailureReleasesCapacityOnRealDaemon(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "failure.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "- branch: feature/storage-blocked"
    git: [[rev-parse, --verify, refs/heads/independent-failure]]
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: clean
      risk_scope: source-or-external
      tested: [fixture]
      testing_summary: fixture
      scenarios: [{name: fixture, result: pass, live: true, evidence: fixture, reason: ""}]
      verdict: go
      artifacts: []
`), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER block_one BEFORE UPDATE OF status ON runs WHEN NEW.branch='feature/storage-blocked' AND NEW.status IN ('failed','cancelled') BEGIN SELECT RAISE(ABORT,'owned storage outage'); END`); err != nil {
		t.Fatal(err)
	}
	defer control.Exec("DROP TRIGGER IF EXISTS block_one")
	h.CommitChange("feature/storage-blocked", "first.txt", "first\n", "first")
	h.PushToGate("feature/storage-blocked")
	first := waitForStepStatus(t, h, "feature/storage-blocked", types.StepReview, types.StepStatusFailed, 30*time.Second)
	h.CommitChange("feature/next-workload", "second.txt", "second\n", "second")
	h.PushToGate("feature/next-workload")
	second := h.WaitForRun("feature/next-workload", 60*time.Second)
	if second.Status != types.RunCompleted {
		t.Fatalf("next workload did not complete: %+v", second)
	}
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(first.ID)
	if err != nil || stored.Status.Terminal() {
		t.Fatalf("blocked run fabricated a durable outcome: %+v %v", stored, err)
	}
	if _, err := os.Stat(stored.WorktreePath()); err != nil {
		t.Fatalf("blocked run lost its worktree custody: %v", err)
	}
	t.Logf("RESULT with first run's storage blocked: second=%s; first=%s, worktree retained", second.Status, stored.Status)
	if _, err := control.Exec("DROP TRIGGER block_one"); err != nil {
		t.Fatal(err)
	}
	settled := h.WaitForRun("feature/storage-blocked", 30*time.Second)
	if settled.Status != types.RunFailed || settled.Error == nil || !strings.Contains(*settled.Error, "claude exited: exit status 1") {
		t.Fatalf("recovery lost independent outcome: %+v", settled)
	}
	t.Logf("RESULT after recovery: first=%s error=%q", settled.Status, *settled.Error)
}

// The #116 fixer requires a positive verification outcome before committing.
// Use the built-in default fake response for that turn and assert the real
// daemon publishes its edit; omitting verification would park without it.
func TestDefaultFixtureVerificationAllowsReviewFixCommit(t *testing.T) {
	scenario := configurableFixCommitScenario(t, "guard unsafe value")
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	realDir := filepath.Join(h.BinDir, "underlying")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	realAgent := filepath.Join(realDir, "claude")
	if err := os.Symlink(h.FakeAgent, realAgent); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(h.BinDir, "claude")
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprompt=$(cat)\ncase \"$prompt\" in *'Investigate previous review findings'*) printf 'safe\\n' > feature.txt; unset FAKEAGENT_SCENARIO ;; esac\nprintf '%s' \"$prompt\" | " + shellQuote(realAgent) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/default-verification"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "unsafe feature")
	h.PushToGate(branch)
	gate := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 30*time.Second)
	h.Respond(gate.ID, types.StepReview, types.ActionFix)
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 30*time.Second)
	h.Respond(gate.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("default verification did not permit completion: %+v", run)
	}
	content, err := h.runGit(context.Background(), h.UpstreamDir, "show", "refs/heads/"+branch+":feature.txt")
	if err != nil || string(content) != "safe\n" {
		t.Fatalf("fix was not committed and published: %v %q", err, content)
	}
	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESULT default fixture verification: run=%s, published feature.txt=%q; commit subjects:\n%s", run.Status, content, log)
}
