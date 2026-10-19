//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The R1 oracle is terminal truth: even when preliminary gate cleanup cannot
// persist, terminal settlement must clear the parked marker and account for its
// elapsed time exactly once. The real daemon owns the terminal transitions;
// the fixture seeds existing parked time, a competing SQLite writer, and an
// on-disk answer, reproducing the answer-before-release race without an IPC hook.
func TestSQLiteParkedGateExhaustionSettlesTerminalTruth(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          reviewConversationScenario(t),
		GlobalConfigExtra: "gate_reconcile_interval: \"100ms\"\ngate_reconcile_timeout: \"2s\"",
	})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)
	statusCommand(t, h, h.WorkDir, "init")
	branch := "feature/parked-storage-failure"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "exercise parked storage failure")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	raw, err := sql.Open("sqlite", filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	since := time.Now().Unix() - 60
	if _, err := raw.Exec(`UPDATE runs SET awaiting_agent_since=?, parked_ms=1500 WHERE id=?`, since, parked.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs='example/repo#92' WHERE id=?`, parked.ID); err != nil {
		t.Fatal(err)
	}
	convDir := reviewqa.Dir(filepath.Join(h.NMHome, "evidence", parked.ID))
	conv, err := reviewqa.Load(convDir)
	if err != nil || len(conv.Open()) != 1 {
		t.Fatalf("expected one scripted reviewer question: %+v %v", conv, err)
	}
	q := conv.Open()[0]
	if err := reviewqa.AppendAnswer(convDir, reviewqa.Answer{ID: q.ID, Answer: "default off", AskOrdinal: 1}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(h.NMHome, "infrastructure-failures", parked.ID+".json")
	// Activity persistence can occupy the single pooled connection before the
	// response, cleanup, and step-failure writers each spend their own budget.
	deadline := time.Now().Add(180 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.dumpDebugState()
			t.Fatal("parked gate never exhausted its writer and preliminary cleanup")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The independent record is emitted only after the earlier cleanup writes.
	// Release now so the final transaction can durably settle the whole run.
	var stillParked int64
	if err := tx.QueryRow(`SELECT awaiting_agent_since FROM runs WHERE id=?`, parked.ID).Scan(&stillParked); err != nil || stillParked != since {
		t.Fatalf("fixture did not retain the failed preliminary cleanup: %d %v", stillParked, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	failed := h.WaitForRun(branch, 20*time.Second)
	if failed.Status != types.RunFailed || failed.Error == nil || !strings.HasPrefix(*failed.Error, "SQLITE_WRITE_CONTENTION:") || failed.AwaitingAgent || failed.AwaitingAgentSince != nil {
		t.Fatalf("terminal IPC truth is still parked or lost its infrastructure identity: %+v", failed)
	}
	step, ok := findStep(failed.Steps, types.StepReview)
	if !ok || step.Status != types.StepStatusFailed {
		t.Fatalf("terminal run stranded its review step: %+v", failed)
	}
	var total int64
	if err := raw.QueryRow(`SELECT parked_ms FROM runs WHERE id=? AND awaiting_agent_since IS NULL`, parked.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	minimum := (time.Now().Unix()-since-2)*1000 + 1500
	if total < minimum || total > minimum+5000 {
		t.Fatalf("lost existing or elapsed parked time: total=%d expected near %d", total, minimum)
	}
	log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil || !strings.Contains(string(log), "failed to complete awaiting-agent state in db") {
		t.Fatalf("preliminary cleanup did not actually fail: %v\n%s", err, log)
	}
	t.Logf("real daemon terminalized a parked review after cleanup exhaustion: awaiting_agent=%v parked_ms=%d (existing 1500ms plus elapsed); error=%s", failed.AwaitingAgent, total, *failed.Error)
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "status"))
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	statusCommand(t, h, h.WorkDir, "daemon", "start")
	recovered := h.RunInfo(parked.ID)
	var after int64
	if err := raw.QueryRow(`SELECT parked_ms FROM runs WHERE id=?`, parked.ID).Scan(&after); err != nil || after != total || recovered.AwaitingAgent || recovered.AwaitingAgentSince != nil || recovered.Status != types.RunFailed {
		t.Fatalf("restart changed terminal accounting: before=%d after=%d run=%+v err=%v", total, after, recovered, err)
	}
	t.Logf("restart retained failed run, awaiting_agent=false and exactly %d parked_ms", after)
}
