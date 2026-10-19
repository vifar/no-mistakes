//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the actual daemon teardown boundary, not a manager with synthetic
// steps: an external agent process fails independently, terminal writes are
// unavailable past the old drain deadline, then storage recovers. The oracle
// requires custody until the original failed outcome is durable.
func TestDaemonRetainsIndependentFailureUntilStorageRecovers(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "independent-failure.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Review the code changes and return structured findings"
    delay_ms: 3000
    git: [[rev-parse, --verify, refs/heads/independent-agent-failure]]
  - structured:
      findings: []
      summary: "fixture"
`), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/durable-independent-failure"
	h.CommitChange(branch, "feature.txt", "exercise durable failure\n", "exercise storage recovery")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 30*time.Second)
	control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER terminal_outage BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT RAISE(FAIL,'disposable terminal storage outage'); END`); err != nil {
		t.Fatal(err)
	}
	defer control.Exec(`DROP TRIGGER IF EXISTS terminal_outage`)
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	deadline := time.Now().Add(15 * time.Second)
	var original string
	for original == "" {
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			if step.StepName == types.StepReview && step.Status == types.StepStatusFailed && step.Error != nil {
				original = *step.Error
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("independent agent failure did not reach terminal storage")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(original, "claude exited: exit status 1") {
		t.Fatalf("not the independent agent-process failure: %s", original)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status.Terminal() {
		t.Fatalf("unsettled outcome: %+v %v", stored, err)
	}
	worktree := stored.WorktreePath()
	pid := daemonPIDForRoot(t, h)
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	// This deliberately exceeds the previous production drain deadline.
	<-time.After(31 * time.Second)
	alive, err := e2edaemon.ProcessAlive(pid)
	if err != nil || !alive {
		t.Fatalf("daemon exited before durable settlement: alive=%t %v", alive, err)
	}
	stored, err = database.GetRun(run.ID)
	if err != nil || stored.Status.Terminal() {
		t.Fatalf("storage outage fabricated terminal truth: %+v %v", stored, err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("shutdown released unsettled worktree: %v", err)
	}
	t.Logf("RESULT after 31s: captured daemon alive, run=%s, worktree retained, independent cause=%q", stored.Status, original)
	if _, err := control.Exec(`DROP TRIGGER terminal_outage`); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not exit after storage recovered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertNoDaemonProcessesForRoot(t, h, "after durable independent failure")
	stored, err = database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, original) {
		t.Fatalf("shutdown lost independent failure: %+v %v", stored, err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("durably settled worktree retained: %v", err)
	}
	t.Logf("RESULT after storage recovery and process exit: run=%s error=%q; custody released", stored.Status, *stored.Error)
}
