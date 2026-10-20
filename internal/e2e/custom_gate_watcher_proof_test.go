//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The certification contract requires every executed custom gate to bind the
// current head, epoch, and config. External agent/forge replies are fixtures;
// execution proof is earned by the real daemon, never inserted by the test.
// Deliberately damaging that persisted proof tests fail-closed cancellation.
func TestCustomGateWatcherProofProductionJourneys(t *testing.T) {
	for _, mode := range []string{"clean", "approved-warning", "formatter-comparison-refusal", "missing-proof", "epoch-drift", "config-drift", "push-generation-drift"} {
		t.Run(mode, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: certifiedWatcherScenario(t)})
			h := j.h
			j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
			gateScript := "#!/bin/sh\nset -eu\ngit rev-parse HEAD >> \"$NM_HOME/gate-heads\"\n"
			if mode == "approved-warning" {
				gateScript += "printf '%s' '{\"findings\":[{\"id\":\"budget\",\"severity\":\"warning\",\"description\":\"near budget\",\"action\":\"ask-user\"}]}' > \"$NO_MISTAKES_FINDINGS_FILE\"\n"
			}
			h.CommitChange("main", "gate.sh", gateScript, "add execution receipt gate")
			trusted := "commands:\n  test: 'true'\n  lint: 'true'\n"
			if mode == "formatter-comparison-refusal" {
				trusted += "  format: 'printf formatted > formatted.txt'\n"
			}
			trusted += "gates:\n  - name: budget\n    after: rebase\n    command: sh gate.sh\n"
			j.setTrusted(trusted)
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					State string `json:"state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				states = append(states, payload.State)
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			branch := "feature/custom-proof-" + mode
			headA := h.CommitChange(branch, "feature.txt", mode+"\n", "exercise custom gate proof")
			h.PushToGate(branch)
			gate := types.CustomGateStepName(types.StepRebase, "budget")
			if mode == "approved-warning" {
				run := waitForStepStatus(t, h, branch, gate, types.StepStatusAwaitingApproval, 30*time.Second)
				h.Respond(run.ID, gate, types.ActionApprove)
			}
			if mode == "formatter-comparison-refusal" {
				// Main's comparison guard refuses this unpublished advance before
				// Review can replay. Preserve that stronger public boundary rather
				// than disabling it to manufacture an otherwise unreachable reset.
				run := h.WaitForRun(branch, 30*time.Second)
				if run.Status != types.RunFailed || !strings.Contains(deref(run.Error), "PR comparison changed after review; start a new run for the current target and head") || run.HeadSHA == headA {
					t.Fatalf("formatter advance did not fail closed: %+v", run)
				}
				database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range publications {
					if p.StrictCertificate != nil || p.LastState == "success" || p.PreserveSuccess != nil && *p.PreserveSuccess {
						t.Fatalf("unvalidated formatter head certified: %+v", p)
					}
				}
				receipt, err := os.ReadFile(filepath.Join(h.NMHome, "gate-heads"))
				if err != nil || string(receipt) != headA+"\n" {
					t.Fatalf("gate did not execute once at A: %q %v", receipt, err)
				}
				out, err := h.Run("axi", "status")
				if err != nil {
					t.Fatalf("status: %v\n%s", err, out)
				}
				t.Logf("RESULT: gate executed at A=%s; formatter created B=%s; comparison guard refused before certification, error=%q\nCLI status:\n%s", headA, run.HeadSHA, deref(run.Error), out)
				return
			}
			run := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusRunning, 45*time.Second)
			control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			deadline := time.Now().Add(15 * time.Second)
			var stored *db.Run
			var publications []db.CommitStatusPublication
			for {
				stored, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				publications, err = database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.CIReadyAt != nil && len(publications) == 1 && publications[0].LastState == "success" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("watcher not ready: %+v %+v", stored, publications)
				}
				time.Sleep(50 * time.Millisecond)
			}
			receipt, err := os.ReadFile(filepath.Join(h.NMHome, "gate-heads"))
			if err != nil || string(receipt) != headA+"\n" {
				t.Fatalf("gate must execute once at head A: %q %v", receipt, err)
			}
			var gateID string
			if err := control.QueryRow(`SELECT id FROM step_results WHERE run_id=? AND step_name=?`, run.ID, gate).Scan(&gateID); err != nil {
				t.Fatal(err)
			}
			gateRow, err := database.GetStepResult(gateID)
			if err != nil || gateRow == nil || gateRow.Status != types.StepStatusCompleted {
				t.Fatalf("custom gate not completed: %+v %v", gateRow, err)
			}
			if publications[0].StrictCertificate == nil {
				t.Fatal("current-head custom gate did not earn a certificate")
			}
			switch mode {
			case "missing-proof":
				_, err = control.Exec(`DELETE FROM status_validation_steps WHERE step_id=?`, gateRow.ID)
			case "epoch-drift":
				_, err = control.Exec(`UPDATE status_validation_steps SET epoch=epoch+1 WHERE step_id=?`, gateRow.ID)
			case "config-drift":
				_, err = control.Exec(`UPDATE status_validation_steps SET config_digest='stale-config' WHERE step_id=?`, gateRow.ID)
			case "push-generation-drift":
				_, err = control.Exec(`UPDATE runs SET push_generation=push_generation+1 WHERE id=?`, run.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			out, err := h.Run("axi", "abort")
			if err != nil {
				t.Fatalf("abort: %v\n%s", err, out)
			}
			terminal := h.WaitForRun(branch, 15*time.Second)
			if terminal.Status != types.RunCancelled || !strings.Contains(deref(terminal.Error), "aborted by user") {
				t.Fatalf("untruthful cancellation: %+v", terminal)
			}
			eligible := mode == "clean" || mode == "approved-warning"
			deadline = time.Now().Add(10 * time.Second)
			for {
				publications, err = database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil || len(publications) != 1 {
					t.Fatalf("terminal publication: %+v %v", publications, err)
				}
				if eligible || publications[0].LastState == "failure" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stale custom proof preserved SUCCESS: %+v", publications[0])
				}
				time.Sleep(50 * time.Millisecond)
			}
			p := publications[0]
			preserved := p.PreserveSuccess != nil && *p.PreserveSuccess
			if preserved != eligible || !eligible && p.StrictCertificate != nil {
				t.Fatalf("wrong preservation eligibility: %+v", p)
			}
			mu.Lock()
			posted := strings.Join(states, ",")
			mu.Unlock()
			wantStates := "pending,success,failure"
			if eligible {
				wantStates = "pending,success"
			}
			if posted != wantStates {
				t.Fatalf("status receipts=%s, want %s", posted, wantStates)
			}
			t.Logf("RESULT: mode=%s executed_gate_head=%s watcher_head=%s cancellation=%s preserved=%t HTTP_states=%s\nabort CLI:\n%s", mode, headA, stored.HeadSHA, terminal.Status, preserved, posted, out)
		})
	}
}
