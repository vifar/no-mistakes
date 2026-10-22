//go:build e2e && !windows

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A planned stop owns an interrupted setup's outcome. Exercise real Git
// checkout and trusted-config fetch processes; a shutdown must persist the
// documented cancellation reason rather than their signal-exit diagnostics.
func TestPlannedStopCancelsRealGitSetup(t *testing.T) {
	for _, stage := range []string{"worktree", "fetch"} {
		t.Run(stage, func(t *testing.T) { exerciseRealGitSetupOutcome(t, stage, true, false) })
		t.Run(stage+"/suspended_group", func(t *testing.T) { exerciseRealGitSetupOutcome(t, stage, true, true) })
	}
}

// An independent checkout or trusted-config fetch failure must remain failed.
// This rejects classifying every setup error as a cancellation and also proves
// that a failed trusted-config fetch never falls through to agent execution.
func TestRealGitSetupFailureKeepsFailedOutcome(t *testing.T) {
	for _, stage := range []string{"worktree", "fetch"} {
		t.Run(stage, func(t *testing.T) { exerciseRealGitSetupOutcome(t, stage, false, false) })
	}
}

func exerciseRealGitSetupOutcome(t *testing.T, stage string, cancelled, suspendGroup bool) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	ready := filepath.Join(h.HomeDir, "setup-started")
	helper := filepath.Join(h.BinDir, "setup-helper")
	body := "exec sleep 300\n"
	if !cancelled {
		body = "echo independent-setup-failure >&2\nexit 23\n"
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" > " + shellQuote(ready) + "\n" + body
	gitConfig := "[filter \"slow\"]\n\tclean = cat\n\tsmudge = " + helper + "\n\trequired = true\n"
	if stage == "fetch" {
		// Local Git transport invokes the configured upload-pack. Block only
		// the worktree's setup fetch, leaving pre-admission reads untouched.
		script = "#!/bin/sh\ncase \"$PWD\" in */worktrees/*)\nprintf '%s\\n' \"$$\" > " + shellQuote(ready) + "\n" + body + ";; esac\nexec git upload-pack \"$@\"\n"
		gitConfig = "[remote \"origin\"]\n\tuploadpack = " + helper + "\n"
	}
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte(gitConfig), 0600); err != nil {
		t.Fatal(err)
	}
	const branch = "feature/cancel-git-setup"
	if stage == "worktree" {
		h.CommitChange(branch, ".gitattributes", "slow.txt filter=slow\n", "configure checkout filter")
	}
	h.CommitChange(branch, "slow.txt", "setup outcome\n", "exercise git setup")
	finishPush := startCustodyPush(t, h, branch)
	awaitCustodyFixture(t, func() bool {
		raw, err := os.ReadFile(ready)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && parseErr == nil && pid > 1
	})
	if suspendGroup {
		// A stopped process cannot handle TERM; cancellation must escalate to
		// KILL. Suspend Git's own process group, discovered from its helper,
		// rather than replacing Git with a sleeper or a shell's exit code.
		raw, err := os.ReadFile(ready)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		group, err := syscall.Getpgid(pid)
		if err != nil || group <= 1 || group == syscall.Getpgrp() {
			t.Fatalf("Git helper's isolated process group: %d %v", group, err)
		}
		if err := syscall.Kill(-group, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer syscall.Kill(-group, syscall.SIGKILL)
	}
	var runID string
	if cancelled {
		run := h.ActiveRun(branch)
		if run == nil {
			t.Fatal("Git setup has no active run")
		}
		runID = run.ID
	} else {
		runID = h.WaitForRun(branch, 30*time.Second).ID
	}
	out, err := h.Run("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("stop after Git setup boundary: %v\n%s", err, out)
	}
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(runID)
	if err != nil || stored == nil {
		t.Fatalf("read settled setup run: %+v %v", stored, err)
	}
	assertNoDaemonProcessesForRoot(t, h, "after Git setup settled")
	finishPush()
	if cancelled {
		if stored.Status != types.RunCancelled || deref(stored.Error) != "cancelled: planned daemon shutdown" {
			t.Fatalf("interrupted Git setup: status=%s error=%q", stored.Status, deref(stored.Error))
		}
	} else if stored.Status != types.RunFailed || !strings.Contains(deref(stored.Error), "independent-setup-failure") {
		t.Fatalf("independent Git setup failure: status=%s error=%q", stored.Status, deref(stored.Error))
	}
	if invocations := h.AgentInvocations(); len(invocations) != 0 {
		t.Fatalf("failed setup reached the agent: %+v", invocations)
	}
	t.Logf("RESULT stage=%s cancelled=%t suspended_group=%t; real Git helper started; stop output=%q; durable status=%s error=%q; no agent started; daemon exited", stage, cancelled, suspendGroup, strings.TrimSpace(out), stored.Status, deref(stored.Error))
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
	control := openControlDB(t, p.DB())
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
