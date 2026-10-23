//go:build e2e

package e2e

import (
	"database/sql"
	"encoding/base64"
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
)

// Drive the real CLI and daemon; the agent boundary is the harness's existing
// silent provider. The independent oracle is the exact operator metadata, read
// from SQLite and from public status after cancellation has stopped the work.
func TestEndingCLIAbortAttributionJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: silentAgentScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/ending-attribution"
	h.CommitChange(branch, "caller.txt", "cancel attribution\n", "add abort probe")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
	if out, err := h.Run("axi", "abort", "--run", run.ID, "--owner", "journey-thread-60", "--reason", "owner priority requires slot"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	d, err := db.OpenReadOnly(h.NMHome + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	var record map[string]any
	_ = json.Unmarshal(raw, &record)
	e, _ := record["EndingEvent"].(map[string]any)
	if stored.Status != types.RunCancelled || e["caller"] != "journey-thread-60" || e["client"] != "axi" || e["reason"] != "owner priority requires slot" {
		t.Fatalf("persisted abort = %s", raw)
	}
	out, err := h.Run("axi", "status", "--run", run.ID, "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var doc struct {
		Run struct {
			Status string         `json:"status"`
			Event  map[string]any `json:"ending_event"`
		} `json:"run"`
	}
	if err = json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, out)
	}
	if doc.Run.Status != "cancelled" || doc.Run.Event["caller"] != "journey-thread-60" {
		t.Fatalf("status loses attribution or reports stale running: %s", out)
	}
	t.Logf("public status: %s", out)
	t.Logf("run %s stopped; SQLite and public JSON identify journey-thread-60, axi, and explicit priority reason", run.ID)
}

// These journeys use the production daemon and IPC handlers. Only the external
// agent is scripted. The oracle is the caller's supplied metadata and issue
// #60's distinction between a received push and a non-push replacement.
func endingJourneyCall(t *testing.T, h *Harness, method string, params, result any) {
	t.Helper()
	c, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.CallWithContext(t.Context(), method, params, result, 15*time.Second); err != nil {
		t.Fatal(err)
	}
}

func endingJourneyRecord(t *testing.T, h *Harness, id string) *db.Run {
	t.Helper()
	d, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	deadline := time.Now().Add(45 * time.Second)
	for {
		r, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.Terminal() && r.EndingEvent != nil {
			raw, _ := json.Marshal(r)
			t.Logf("persisted run: %s", raw)
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never settled: %+v", id, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func endingJourneyAgentStarted(t *testing.T, h *Harness, id, prompt string) {
	t.Helper()
	waitSQLiteJourney(t, h, "external agent started for "+id, func() bool {
		for _, inv := range h.AgentInvocations() {
			if strings.Contains(inv.CWD, id) && strings.Contains(inv.Prompt, prompt) {
				return true
			}
		}
		return false
	})
}

func endingJourneyOption(key, value string) string {
	return "--push-option=no-mistakes." + key + "=" + base64.StdEncoding.EncodeToString([]byte(value))
}

func TestEndingPublicCompatibilityJourney(t *testing.T) {
	for _, mode := range []string{"legacy", "launch_owner", "queued", "disabled_info_logs"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Scenario: silentAgentScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n"})
			if mode == "disabled_info_logs" {
				p := filepath.Join(h.NMHome, "config.yaml")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(strings.Replace(string(b), "log_level: debug", "log_level: error", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			statusCommand(t, h, h.WorkDir, "init")
			if mode == "queued" {
				h.CommitChange("feature/holder", "holder.txt", "hold\n", "hold slot")
				h.PushToGate("feature/holder")
				waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusRunning, 90*time.Second)
			}
			const branch = "feature/compatibility"
			h.CommitChange(branch, "feature.txt", "compatibility\n", "probe cancellation")
			if mode == "launch_owner" {
				out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch, endingJourneyOption("owner", "launch-thread-60"))
				if err != nil {
					t.Fatalf("owned push: %v %s", err, out)
				}
			} else {
				h.PushToGate(branch)
			}
			var id string
			if mode == "queued" {
				r := h.ActiveRun(branch)
				if r == nil || r.Status != types.RunQueued {
					t.Fatalf("not queued: %+v", r)
				}
				id = r.ID
			} else {
				id = waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second).ID
				endingJourneyAgentStarted(t, h, id, "Review the code changes")
			}
			params := map[string]any{"run_id": id}
			if mode == "queued" || mode == "disabled_info_logs" {
				params["caller"], params["reason"], params["client"] = "compat-thread", "explicit compatibility stop", "journey-ipc"
			}
			endingJourneyCall(t, h, ipc.MethodCancelRun, params, nil)
			r := endingJourneyRecord(t, h, id)
			e := r.EndingEvent
			if r.Status != types.RunCancelled || deref(r.Error) != types.RunCancelReasonAbortedByUser || e == nil {
				t.Fatalf("cancel outcome changed: %+v", r)
			}
			if mode == "legacy" && (e.Caller != "" || e.AttributionSource != "unavailable") {
				t.Fatalf("invented legacy identity: %+v", e)
			}
			if mode == "launch_owner" && (e.Caller != "launch-thread-60" || e.AttributionSource != "launch_owner") {
				t.Fatalf("fallback mislabelled: %+v", e)
			}
			if mode == "queued" || mode == "disabled_info_logs" {
				if e.Caller != "compat-thread" || e.Reason != "explicit compatibility stop" || e.Client != "journey-ipc" {
					t.Fatalf("explicit metadata lost: %+v", e)
				}
			}
			if h.RunInfo(id).EndingEvent == nil {
				t.Fatal("get_run omitted event")
			}
			found := false
			for _, listed := range h.Runs() {
				if listed.ID == id {
					found = listed.EndingEvent != nil
				}
			}
			if !found {
				t.Fatal("list omitted event")
			}
			t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", id, "--json"))
		})
	}
}

func TestEndingPublicSupersessionJourney(t *testing.T) {
	for _, state := range []string{"queued", "admitted"} {
		for _, trigger := range []string{"push", "rerun", "fresh"} {
			t.Run(state+"/"+trigger, func(t *testing.T) {
				h := NewHarness(t, SetupOpts{Scenario: silentAgentScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n"})
				statusCommand(t, h, h.WorkDir, "init")
				if state == "queued" {
					h.CommitChange("feature/holder", "holder.txt", "hold\n", "hold slot")
					h.PushToGate("feature/holder")
					waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusRunning, 90*time.Second)
				}
				const branch = "feature/supersession"
				head := h.CommitChange(branch, "feature.txt", "first\n", "first version")
				h.PushToGate(branch)
				old := h.ActiveRun(branch)
				if state == "admitted" {
					old = waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
					endingJourneyAgentStarted(t, h, old.ID, "Review the code changes")
				} else if old == nil || old.Status != types.RunQueued {
					t.Fatalf("not queued: %+v", old)
				}
				var replacement, newHead string
				switch trigger {
				case "push":
					newHead = h.CommitChange(branch, "feature.txt", "second\n", "second version")
					out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch, endingJourneyOption("owner", "replacement-owner"), endingJourneyOption("launch-nonce", "ending-nonce"), endingJourneyOption("validation-generation", "ending-generation"), endingJourneyOption("intent", "replace work"))
					if err != nil {
						t.Fatalf("replacement push: %v %s", err, out)
					}
					replacement = h.ActiveRun(branch).ID
				case "rerun":
					var result ipc.RerunResult
					endingJourneyCall(t, h, ipc.MethodRerun, &ipc.RerunParams{RepoID: h.repoID(), Branch: branch, PreviousRunID: old.ID}, &result)
					replacement = result.RunID
				case "fresh":
					var result ipc.StartFreshRunResult
					endingJourneyCall(t, h, ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{RepoID: h.repoID(), Branch: branch, HeadSHA: head, LaunchNonce: "fresh-nonce", ValidationGeneration: "fresh-generation", Intent: "replace work"}, &result)
					if result.Receipt.RunID == "" {
						t.Fatal("no fresh receipt")
					}
					replacement = result.Receipt.RunID
				}
				r := endingJourneyRecord(t, h, old.ID)
				e := r.EndingEvent
				if r.Status != types.RunCancelled || deref(r.Error) != types.RunCancelReasonSuperseded || e == nil || e.Event != "run_superseded" || e.SupersedingRunID != replacement || e.AttributionSource != trigger || e.SupersedingGeneration != nil {
					t.Fatalf("supersession outcome/provenance: %+v %+v", r, e)
				}
				if trigger == "push" {
					if e.SupersedingSHA != newHead || e.SupersedingOldSHA != head || e.SupersedingRef != "refs/heads/"+branch || e.PushNotificationID == "" || deref(e.SupersedingValidationGeneration) != "ending-generation" {
						t.Fatalf("received push lost: %+v", e)
					}
					t.Log(statusCommand(t, h, h.WorkDir, "daemon", "notifications", "inspect", e.PushNotificationID))
				} else if e.SupersedingSHA != "" || e.SupersedingOldSHA != "" || e.SupersedingRef != "" || e.PushNotificationID != "" {
					t.Fatalf("invented push: %+v", e)
				}
			})
		}
	}
}

func TestEndingPublicSchemaMigrationJourney(t *testing.T) {
	for _, version := range []int{0, 5, 6} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Scenario: silentAgentScenario(t)})
			statusCommand(t, h, h.WorkDir, "init")
			h.CommitChange("feature/legacy", "feature.txt", "legacy\n", "legacy row")
			h.PushToGate("feature/legacy")
			r := waitForStepStatus(t, h, "feature/legacy", types.StepReview, types.StepStatusRunning, 90*time.Second)
			statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
			raw, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var currentSchema int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&currentSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(fmt.Sprintf("ALTER TABLE runs DROP COLUMN ending_event; PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			statusCommand(t, h, h.WorkDir, "daemon", "start")
			var stamp int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&stamp); err != nil || stamp != currentSchema {
				t.Fatalf("schema stamp=%d want=%d err=%v", stamp, currentSchema, err)
			}
			info := h.RunInfo(r.ID)
			if info.EndingEvent != nil || info.HeadSHA != r.HeadSHA {
				t.Fatalf("migration fabricated attribution or changed head: %+v", info)
			}
			t.Logf("schema %d migrated to %d; old run has no invented event", version, stamp)
			t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", r.ID, "--json"))
		})
	}
}

func TestEndingPublicMidAutoFixJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "ending-autofix.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Fix the failing tests in this repository"
    delay_ms: 60000
    text: "never reached"
  - match: "You are validating a code change by driving the product itself"
    structured:
      findings:
        - id: fixture-failure
          severity: error
          description: "disposable external test failure"
          action: auto-fix
      summary: "fixture failure requires repair"
      tested: ["fixture command"]
      testing_summary: "fixture failure"
      scenarios:
        - name: "external fixture failure"
          result: fail
          live: true
          evidence: "fixture command"
          reason: ""
      verdict: no-go
      artifacts: []
  - structured:
      findings: []
      summary: "clean review"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
`), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Scenario: scenario})
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nauto_fix:\n  test: 3\n", "enable test repair budget")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("trusted configuration: %v %s", err, out)
	}
	statusCommand(t, h, h.WorkDir, "init")
	const branch = "feature/mid-auto-fix"
	h.CommitChange(branch, "feature.txt", "repair probe\n", "exercise auto-fix abort")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusFixing, 90*time.Second)
	endingJourneyAgentStarted(t, h, run.ID, "Fix the failing tests in this repository")
	log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil || !strings.Contains(string(log), `msg="auto-fixing step" step=test attempt=1 max=3`) {
		t.Fatalf("not the measured first repair attempt: %v %s", err, log)
	}
	t.Log("daemon phase: auto-fixing step test attempt=1 max=3; external repair process is running")
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "abort", "--run", run.ID, "--owner", "Firstmate-thread", "--reason", "owner priority"))
	r := endingJourneyRecord(t, h, run.ID)
	if r.Status != types.RunCancelled || r.EndingEvent == nil || r.EndingEvent.Caller != "Firstmate-thread" || r.EndingEvent.Reason != "owner priority" || r.EndingEvent.Client != "axi" {
		t.Fatalf("mid-repair caller/reason lost: %+v", r)
	}
	log, err = os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil || !strings.Contains(string(log), "caller=Firstmate-thread") || !strings.Contains(string(log), `reason="owner priority"`) {
		t.Fatalf("daemon caller/reason lost: %v %s", err, log)
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "run ending requested") {
			t.Log(line)
		}
	}
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", run.ID, "--json"))
}

// A rejected terminal write releases the completed workload's slot while
// retaining custody for settlement. Abort after that public boundary is a
// no-op: it must return promptly and never invent cancellation attribution.
// The successful-write race below separately covers a request accepted while
// the initial terminal write is still in flight.
func TestEndingPublicTerminalOutageJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "ending-independent-failure.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Review the code changes and return structured findings"
    delay_ms: 3000
    git: [[rev-parse, --verify, refs/heads/independent-ending-failure]]
  - structured:
      findings: []
      summary: "fixture"
`), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Scenario: scenario})
	statusCommand(t, h, h.WorkDir, "init")
	const branch = "feature/terminal-outage"
	h.CommitChange(branch, "feature.txt", "terminal probe\n", "exercise late cancellation")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
	endingJourneyAgentStarted(t, h, run.ID, "Review the code changes")
	control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER ending_terminal_outage BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT RAISE(FAIL,'disposable ending terminal outage'); END`); err != nil {
		t.Fatal(err)
	}
	defer control.Exec("DROP TRIGGER IF EXISTS ending_terminal_outage")
	waitSQLiteJourney(t, h, "terminal write attempted before cancel", func() bool {
		log, _ := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
		return strings.Contains(string(log), `msg="retrying terminal outcome settlement"`)
	})
	gateJourneyAwaitSlots(t, h, func(slots ipc.SlotsResult) bool { return slots.InUse == 0 })
	if stored := h.RunInfo(run.ID); stored.Status.Terminal() || stored.EndingEvent != nil {
		t.Fatalf("outage did not retain unsettled custody: %+v", stored)
	}
	if _, err := os.Stat(paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)); err != nil {
		t.Fatalf("outage lost the unsettled worktree: %v", err)
	}
	start := time.Now()
	endingJourneyCall(t, h, ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: run.ID, Caller: "late-terminal-caller", Client: "journey-ipc", Reason: "stop while settlement is unavailable"}, nil)
	if time.Since(start) > time.Second {
		t.Fatal("cancellation waited for terminal storage")
	}
	t.Logf("cancel_run returned in %s while terminal writes were refused", time.Since(start))
	if _, err := control.Exec("DROP TRIGGER ending_terminal_outage"); err != nil {
		t.Fatal(err)
	}
	var r *ipc.RunInfo
	waitSQLiteJourney(t, h, "independent failure settled after storage recovered", func() bool {
		r = h.RunInfo(run.ID)
		return r.Status.Terminal()
	})
	if r.Status != types.RunFailed || !strings.Contains(deref(r.Error), "claude exited: exit status 1") || r.EndingEvent != nil {
		t.Fatalf("late abort replaced the independent failure or fabricated attribution: %+v", r)
	}
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "abort", "--run", run.ID, "--owner", "too-late", "--reason", "must not overwrite"))
	after := h.RunInfo(run.ID)
	if after.Status != types.RunFailed || deref(after.Error) != deref(r.Error) || after.EndingEvent != nil {
		t.Fatal("terminal abort changed the outcome or fabricated attribution")
	}
	log, _ := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if strings.Contains(string(log), "caller=late-terminal-caller") || strings.Contains(string(log), "caller=too-late") {
		t.Fatal("closed cancellation acceptance fabricated an audit")
	}
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", run.ID, "--json"))
}

// The recursive SQL is a disposable delay inside an otherwise successful
// terminal UPDATE. A second writer's SQLITE_BUSY response proves the production
// daemon holds that write, after its preceding step result is durable. Unlike
// the outage journey, this reproduces the single-write/no-retry R4 window.
func TestEndingPublicSuccessfulTerminalWriteRaceJourney(t *testing.T) {
	for _, outcome := range []string{"failure", "success"} {
		t.Run(outcome, func(t *testing.T) {
			scenario := filepath.Join(t.TempDir(), "ending-terminal-delay.yaml")
			body := "actions:\n  - match: \"Review the code changes and return structured findings\"\n    delay_ms: 3000\n"
			if outcome == "failure" {
				body += "    git: [[rev-parse, --verify, refs/heads/independent-ending-failure]]\n"
			} else {
				body += "    structured:\n      findings: []\n      summary: clean\n      risk_level: low\n      risk_rationale: fixture\n      risk_scope: source-or-external\n"
			}
			// The default scenario supplies the remaining external-agent answers.
			defaultPath := silentAgentScenario(t)
			defaultBytes, err := os.ReadFile(defaultPath)
			if err != nil {
				t.Fatal(err)
			}
			body += string(defaultBytes[strings.Index(string(defaultBytes), "  - text: \"no issues found\""):])
			if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Scenario: scenario})
			statusCommand(t, h, h.WorkDir, "init")
			const branch = "feature/terminal-write-race"
			h.CommitChange(branch, "feature.txt", "terminal race\n", "exercise successful terminal write")
			h.PushToGate(branch)
			run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
			endingJourneyAgentStarted(t, h, run.ID, "Review the code changes")
			control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			control.SetMaxOpenConns(1)
			if _, err := control.Exec(`CREATE TRIGGER ending_terminal_delay BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','completed') BEGIN SELECT sum(x) FROM (WITH RECURSIVE counter(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM counter WHERE x<3000000) SELECT x FROM counter); END`); err != nil {
				t.Fatal(err)
			}
			waitSQLiteJourney(t, h, "otherwise successful terminal write holds SQLite writer", func() bool {
				var status string
				step := "review"
				if outcome == "success" {
					step = "ci"
				}
				if err := control.QueryRow("SELECT status FROM step_results WHERE run_id=? AND step_name=?", run.ID, step).Scan(&status); err != nil || status == "running" || status == "pending" {
					return false
				}
				_, err := control.Exec("BEGIN IMMEDIATE")
				if err == nil {
					_, _ = control.Exec("ROLLBACK")
					return false
				}
				return strings.Contains(err.Error(), "SQLITE_BUSY")
			})
			start := time.Now()
			endingJourneyCall(t, h, ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: run.ID, Caller: "terminal-write-caller", Client: "journey-ipc", Reason: "accepted while terminal UPDATE is blocked"}, nil)
			if time.Since(start) > time.Second {
				t.Fatal("cancellation blocked behind the terminal database write")
			}
			t.Logf("otherwise successful %s UPDATE held SQLite writer; cancel_run returned in %s", outcome, time.Since(start))
			r := endingJourneyRecord(t, h, run.ID)
			want := types.RunFailed
			if outcome == "success" {
				want = types.RunCompleted
			}
			if r.Status != want || r.EndingEvent == nil || r.EndingEvent.Caller != "terminal-write-caller" || r.EndingEvent.Reason != "accepted while terminal UPDATE is blocked" {
				t.Fatalf("accepted cancellation lost or independent outcome changed: %+v", r)
			}
			if outcome == "failure" && !strings.Contains(deref(r.Error), "claude exited: exit status 1") {
				t.Fatalf("independent failure changed: %+v", r)
			}
			t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", run.ID, "--json"))
		})
	}
}

func TestEndingPublicClosedAcceptanceJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{})
	statusCommand(t, h, h.WorkDir, "init")
	const branch = "feature/closed-acceptance"
	h.CommitChange(branch, "feature.txt", "completed work\n", "exercise late abort")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted || run.EndingEvent != nil {
		t.Fatalf("run did not complete without a cancellation: %+v", run)
	}
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "abort", "--run", run.ID, "--owner", "too-late", "--reason", "must not create audit"))
	after := h.RunInfo(run.ID)
	if after.Status != types.RunCompleted || after.EndingEvent != nil {
		t.Fatalf("late abort changed outcome or fabricated an audit: %+v", after)
	}
	log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil || strings.Contains(string(log), "caller=too-late") {
		t.Fatalf("late abort logged as accepted: %v %s", err, log)
	}
	t.Log(statusCommand(t, h, h.WorkDir, "axi", "status", "--run", run.ID, "--json"))
}
