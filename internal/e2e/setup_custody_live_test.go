//go:build e2e && !windows

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A real Git checkout blocked in a smudge filter is setup work, not an
// independent failure. Planned stop must preserve the documented cancellation
// outcome even though Git reports a signal exit through its transport wrapper.
func TestPlannedStopCancelsRealGitSetup(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ready := filepath.Join(h.HomeDir, "smudge-started")
	filter := filepath.Join(h.BinDir, "slow-smudge")
	if err := os.WriteFile(filter, []byte("#!/bin/sh\nprintf ready > "+shellQuote(ready)+"\nexec sleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[filter \"slow\"]\n\tsmudge = "+filter+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const branch = "feature/cancel-git-setup"
	h.CommitChange(branch, ".gitattributes", "slow.txt filter=slow\n", "configure checkout filter")
	h.CommitChange(branch, "slow.txt", "checkout is cancellable\n", "exercise git setup")
	finishPush := startCustodyPush(t, h, branch)
	awaitCustodyFixture(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("Git setup has no active run")
	}
	out, err := h.Run("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("stop during Git setup: %v\n%s", err, out)
	}
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(run.ID)
	if err != nil || stored == nil {
		t.Fatalf("read settled setup run: %+v %v", stored, err)
	}
	if stored.Status != types.RunCancelled || deref(stored.Error) != "cancelled: planned daemon shutdown" {
		t.Fatalf("Git setup cancellation: status=%s error=%q", stored.Status, deref(stored.Error))
	}
	assertNoDaemonProcessesForRoot(t, h, "after cancelling Git setup")
	finishPush()
	t.Logf("RESULT real Git smudge filter started; stop output=%q; durable status=%s error=%q", strings.TrimSpace(out), stored.Status, deref(stored.Error))
}

// An admission failure never gets a cancellation handle. Its still-owned
// outcome must nevertheless prevent a successful shutdown acknowledgement.
func TestPlannedStopRefusesOwnedAdmissionFailureOnRealDaemon(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          silentAgentScenario(t),
		GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("feature/slot-holder", "holder.txt", "hold\n", "hold capacity")
	h.PushToGate("feature/slot-holder")
	waitForStepStatus(t, h, "feature/slot-holder", types.StepReview, types.StepStatusRunning, 30*time.Second)
	p := paths.WithRoot(h.NMHome)
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TABLE admission_attempts (status TEXT)`); err != nil {
		t.Fatal(err)
	}
	// FAIL preserves the observation INSERT from this autocommit statement
	// while refusing the outcome UPDATE. The queue transaction rolls its own
	// observation back, so a failed observation proves settlement was reached.
	if _, err := control.Exec(`CREATE TRIGGER admission_outage BEFORE UPDATE OF status ON runs WHEN NEW.branch='feature/unadmitted' AND NEW.status IN ('queued','failed','cancelled') BEGIN INSERT INTO admission_attempts VALUES(NEW.status); SELECT RAISE(FAIL,'owned admission storage outage'); END`); err != nil {
		t.Fatal(err)
	}
	defer control.Exec("DROP TRIGGER IF EXISTS admission_outage")
	h.CommitChange("feature/unadmitted", "second.txt", "second\n", "exercise admission failure")
	finishPush := startCustodyPush(t, h, "feature/unadmitted")
	awaitCustodyFixture(t, func() bool {
		var attempts int
		err := control.QueryRow(`SELECT COUNT(*) FROM admission_attempts WHERE status='failed'`).Scan(&attempts)
		return err == nil && attempts > 0
	})
	run := h.ActiveRun("feature/unadmitted")
	if run == nil || run.Status != types.RunPending {
		t.Fatalf("unadmitted run: %+v", run)
	}
	data, err := os.ReadFile(p.PIDFile())
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Instance  string    `json:"instance"`
		PID       int       `json:"pid"`
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.ShutdownParams{ExpectedPID: identity.PID, ExpectedInstance: identity.Instance, ExpectedStartedAt: identity.StartedAt.UTC().Format(time.RFC3339Nano)}
	var ack ipc.ShutdownResult
	err = client.CallWithTimeout(ipc.MethodShutdown, &request, &ack, 15*time.Second)
	if err == nil || ack.OK || !strings.Contains(err.Error(), "unsettled run "+run.ID) {
		t.Fatalf("shutdown acknowledged unsettled admission failure: %+v %v", ack, err)
	}
	var health ipc.HealthResult
	if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil || health.Status != "ok" {
		t.Fatalf("refused shutdown lost daemon custody: %+v %v", health, err)
	}
	t.Logf("RESULT shutdown refused: %v; daemon health=%s; owned run=%s", err, health.Status, run.Status)
	if _, err := control.Exec("DROP TRIGGER admission_outage"); err != nil {
		t.Fatal(err)
	}
	stored := h.WaitForRun("feature/unadmitted", 30*time.Second)
	if stored.Status != types.RunFailed || !strings.Contains(deref(stored.Error), "queue run:") {
		t.Fatalf("recovered admission outcome: %+v", stored)
	}
	finishPush()
	t.Logf("RESULT storage recovered: run=%s error=%q", stored.Status, deref(stored.Error))
}

// The receive hook waits for setup, so a test that blocks setup must retain
// and join the push while driving the daemon from a separate connection.
func startCustodyPush(t *testing.T, h *Harness, branch string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, "git", "push", "no-mistakes", branch)
	cmd.Dir = h.WorkDir
	shellenv.ConfigureShellCommand(cmd)
	done := make(chan struct{})
	var output []byte
	var err error
	go func() {
		output, err = shellenv.CombinedOutputShellCommand(cmd)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("fixture push did not exit")
		}
	})
	return func() {
		t.Helper()
		select {
		case <-done:
			t.Logf("receive hook returned: error=%v output=%s", err, strings.TrimSpace(string(output)))
		case <-time.After(15 * time.Second):
			t.Fatal("receive hook did not finish after setup settled")
		}
	}
}

func awaitCustodyFixture(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("custody fixture did not reach its readiness boundary")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
