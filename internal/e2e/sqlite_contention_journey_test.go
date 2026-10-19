//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Real CLI/daemon/SQLite/Git, with the existing scripted agent and local HTTP
// forge boundary. The oracle is #92: a competing closing-issue write survives,
// and infrastructure exhaustion adds neither success nor failure status POSTs.
func TestSQLiteContentionOnRealDaemon(t *testing.T) {
	for _, mode := range []string{"release_after_timeout", "exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			h, forge := newStatusJourney(t, "github")
			cleanSQLiteJourneyScenario(t, h)
			branch, head, wt := statusBranch(t, h, true)
			// The first state query precedes the scope write. The second is the
			// monitor's observation, immediately before UpdateRunPRState.
			gh := filepath.Join(h.BinDir, "gh")
			delegate := filepath.Join(h.BinDir, "lock-gh-delegate")
			if err := os.Rename(gh, delegate); err != nil {
				t.Fatal(err)
			}
			first, ready, resume := gh+".first", gh+".ready", gh+".resume"
			script := "#!/bin/sh\ncase \"$*\" in\n*'--json state --jq .state'*)\n" +
				"if [ ! -f " + shellQuote(first) + " ]; then : > " + shellQuote(first) + "; else\n" +
				": > " + shellQuote(ready) + "\nn=0\nwhile [ ! -f " + shellQuote(resume) + " ]; do\n" +
				"n=$((n+1)); [ \"$n\" -lt 600 ] || exit 91; sleep 0.1\ndone\nfi;;\nesac\n" +
				"exec " + shellQuote(delegate) + " \"$@\"\n"
			if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			h.PushToGate(branch)
			waitSQLiteJourney(t, h, "CI observation barrier", func() bool { _, err := os.Stat(ready); return err == nil })
			run := h.ActiveRun(branch)
			if run == nil {
				t.Fatal("no active CI run")
			}
			ci, ok := findStep(run.Steps, types.StepCI)
			if !ok || ci.Status != types.StepStatusRunning {
				t.Fatalf("barrier outside CI: %+v", run)
			}
			before, err := json.Marshal(forge.history(head))
			if err != nil || len(forge.history(head)) == 0 {
				t.Fatalf("no preexisting published status: %s %v", before, err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			raw.SetMaxOpenConns(1)
			tx, err := raw.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs=? WHERE id=?`, `example/closes#92`, run.ID); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if err := os.WriteFile(resume, nil, 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(h.NMHome, "infrastructure-failures", run.ID+".json")
			if mode == "release_after_timeout" {
				time.Sleep(6 * time.Second)
			} else {
				// Do not block terminal persistence after the exhausted writer.
				waitSQLiteJourney(t, h, "independent infrastructure record", func() bool { _, err := os.Stat(marker); return err == nil })
				if time.Since(started) < 30*time.Second || time.Since(started) > 38*time.Second {
					t.Fatalf("unbounded or premature exhaustion: %v", time.Since(started))
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if mode == "release_after_timeout" {
				waitSQLiteJourney(t, h, "PR-state write after transient lock", func() bool {
					var state string
					var observed int64
					return raw.QueryRow(`SELECT COALESCE(pr_state,''), COALESCE(pr_state_observed_at,0) FROM runs WHERE id=?`, run.ID).Scan(&state, &observed) == nil && state == "open" && observed >= started.Unix()+5
				})
				info := h.RunInfo(run.ID)
				if info.Status != types.RunRunning || info.Error != nil {
					t.Fatalf("transient contention failed the running monitor: %+v", info)
				}
				t.Log(statusCommand(t, h, wt, "axi", "status"))
			} else {
				failed := h.WaitForRun(branch, 20*time.Second)
				if failed.Status != types.RunFailed || failed.Error == nil || !strings.Contains(*failed.Error, "SQLITE_WRITE_CONTENTION: infrastructure failure writing UpdateRunPRState after ") {
					t.Fatalf("wrong terminal identity: %+v", failed)
				}
				ci, _ := findStep(failed.Steps, types.StepCI)
				if ci.Status != types.StepStatusFailed || failed.AwaitingAgent || failed.AwaitingAgentSince != nil {
					t.Fatalf("stranded active step or parked marker: %+v", failed)
				}
				after, _ := json.Marshal(forge.history(head))
				if string(after) != string(before) {
					t.Fatalf("exhaustion altered status: before=%s after=%s", before, after)
				}
				t.Logf("exhausted after %v; existing HTTP status history unchanged: %s", time.Since(started), after)
				t.Log(statusCommand(t, h, wt, "axi", "status"))
			}
			var state, refs string
			if err := raw.QueryRow(`SELECT COALESCE(pr_state,''), closing_issue_refs FROM runs WHERE id=?`, run.ID).Scan(&state, &refs); err != nil {
				t.Fatal(err)
			}
			if refs != `example/closes#92` || mode == "release_after_timeout" && state != "open" {
				t.Fatalf("lost writer: state=%q refs=%q", state, refs)
			}
			t.Logf("real daemon after %v: pr_state=%q closing_issue_refs=%s", time.Since(started), state, refs)
			if mode != "exhaustion" {
				return
			}
			statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
			// Simulate lost terminal SQLite truth, and ordinary log retention.
			// Only the independent record may protect recovery in this state.
			if _, err := raw.Exec(`UPDATE runs SET error='daemon crashed' WHERE id=?`, run.ID); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(h.NMHome, "logs", run.ID)); err != nil {
				t.Fatal(err)
			}
			for _, evidence := range []string{"retained", "corrupt", "unreadable"} {
				if evidence == "corrupt" {
					if err := os.WriteFile(marker, []byte("{broken"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if evidence == "unreadable" {
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(marker, 0700); err != nil {
						t.Fatal(err)
					}
				}
				statusCommand(t, h, h.WorkDir, "daemon", "start")
				after, _ := json.Marshal(forge.history(head))
				if string(after) != string(before) {
					t.Fatalf("%s recovery fabricated status: %s", evidence, after)
				}
				log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
				if err != nil {
					t.Fatal(err)
				}
				if evidence != "retained" {
					want := "SQLITE_INFRASTRUCTURE_RECORD_INVALID"
					if evidence == "unreadable" {
						want = "SQLITE_INFRASTRUCTURE_RECORD_UNREADABLE"
					}
					if !strings.Contains(string(log), want) {
						t.Fatalf("recovery hid %s evidence: %s", evidence, log)
					}
					t.Logf("restart with %s record emitted %s; status POST history unchanged", evidence, want)
				} else {
					t.Log("restart after run-log removal retained infrastructure identity and made no new status POST")
				}
				statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
			}
		})
	}
}

func waitSQLiteJourney(t *testing.T, h *Harness, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(50 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("timed out waiting for %s", what)
}

// A failing command mentioning the storage token is still a source failure.
func TestSQLiteTokenInFailingCommandOnRealDaemon(t *testing.T) {
	h, forge := newStatusJourney(t, "github")
	cleanSQLiteJourneyScenario(t, h)
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\ncommands:\n  prepare: 'echo SQLITE_WRITE_CONTENTION: command failure; exit 7'\n  test: 'true'\n", "configure genuine failure control")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("publish trusted command: %v\n%s", err, out)
	}
	branch, head, wt := statusBranch(t, h, true)
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || strings.HasPrefix(*run.Error, "SQLITE_WRITE_CONTENTION:") {
		t.Fatalf("command failure adopted infrastructure identity: %+v", run)
	}
	forge.assert(t, head, "pending", "failure")
	commandLog, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "test.log"))
	if err != nil || !strings.Contains(string(commandLog), "SQLITE_WRITE_CONTENTION: command failure") {
		t.Fatalf("failing command did not emit the control token: %v\n%s", err, commandLog)
	}
	t.Logf("real command output:\n%s", commandLog)
	t.Log(statusCommand(t, h, wt, "axi", "status"))
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	statusCommand(t, h, h.WorkDir, "daemon", "start")
	posts := forge.history(head)
	if len(posts) == 0 || posts[len(posts)-1].State != "failure" {
		t.Fatalf("restart suppressed genuine failure: %+v", posts)
	}
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed {
		t.Fatalf("genuine failure not durable: %+v %v", stored, err)
	}
}

func cleanSQLiteJourneyScenario(t *testing.T, h *Harness) {
	t.Helper()
	clean, err := os.ReadFile(h.Scenario)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(clean), "\n  - text: \"no issues found\"")
	if start < 0 {
		t.Fatal("clean scenario action missing")
	}
	if err := os.WriteFile(h.Scenario, []byte("actions:"+string(clean[start:])), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteRecordClearedOnRealGateRecovery(t *testing.T) {
	h, forge := newStatusJourney(t, "github")
	branch, head, wt := statusBranch(t, h, true)
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	dir := filepath.Join(h.NMHome, "infrastructure-failures")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, run.ID+".json")
	record, err := json.Marshal(map[string]string{
		"run_id": run.ID,
		"error":  "SQLITE_WRITE_CONTENTION: infrastructure failure writing prior attempt after 2 attempts: database is locked (5) (SQLITE_BUSY)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, record, 0600); err != nil {
		t.Fatal(err)
	}
	statusCommand(t, h, h.WorkDir, "daemon", "start")
	waitSQLiteJourney(t, h, "fresh execution clearing prior infrastructure record", func() bool {
		_, err := os.Stat(marker)
		return os.IsNotExist(err)
	})
	if current := h.ActiveRun(branch); current == nil || current.ID != run.ID {
		t.Fatalf("recovery replaced the run: %+v", current)
	}
	statusCommand(t, h, wt, "axi", "abort")
	posts := forge.history(head)
	if len(posts) == 0 || posts[len(posts)-1].State != "failure" {
		t.Fatalf("prior infrastructure record suppressed the new outcome: %+v", posts)
	}
	t.Logf("real daemon resumed the same run %s, removed its prior infrastructure record, and reported the later abort as failure", run.ID)
}
