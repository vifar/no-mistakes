//go:build e2e && !windows

package e2e

import (
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

// The oracle is the watcher cancellation contract: actually executed clean
// gates and acknowledged SUCCESS may survive cancellation; an approved error,
// uncertain acknowledgement, or changed worktree may not. Only the external
// agent/forge boundaries are fixtures. No executed proof or certificate is seeded.
func TestWatcherCertificateBoundaryJourneys(t *testing.T) {
	for _, tc := range []struct {
		name, mode                               string
		recovered, uncertain, dirty, changedHead bool
		repair                                   bool
		eligible                                 bool
	}{
		{name: "clean", mode: "legacy", eligible: true},
		{name: "approved-warning", mode: "warning", eligible: true},
		{name: "approved-error", mode: "error-zero"},
		{name: "recovered-approved-error", mode: "error-zero", recovered: true},
		{name: "approved-malformed", mode: "malformed"},
		{name: "approved-transport-refusal", mode: "transport"},
		{name: "uncertain-success", mode: "legacy", uncertain: true},
		{name: "dirty-worktree", mode: "legacy", dirty: true},
		{name: "changed-head", mode: "legacy", changedHead: true},
		{name: "same-head-noop-repair", mode: "legacy", repair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := certifiedWatcherScenario(t)
			if tc.repair {
				content, err := os.ReadFile(scenario)
				if err != nil {
					t.Fatal(err)
				}
				content = []byte(strings.Replace(string(content), "actions:\n", `actions:
  - match: "The following CI checks have failed"
    structured:
      code_change_needed: false
      summary: "external failure; no code change"
`, 1))
				if err := os.WriteFile(scenario, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: scenario})
			h := j.h
			j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
			h.CommitChange("main", "scripts/gate.sh", structuredGateScript, "add executable gate")
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\ngates:\n  - name: budget\n    after: lint\n    command: sh scripts/gate.sh 0\n")
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
				if tc.uncertain && payload.State == "success" {
					http.Error(w, "status acknowledgement unavailable", 503)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			branch := "feature/certificate-" + tc.name
			head := h.CommitChange(branch, "gate-mode.txt", tc.mode+"\n", "exercise watcher certificate boundary")
			// Ordinary approval remains available for unresolved custom gates;
			// it must not be mistaken for executed clean certification.
			h.PushToGate(branch)
			gate := types.CustomGateStepName(types.StepLint, "budget")
			if tc.mode != "legacy" {
				parked := waitForStepStatus(t, h, branch, gate, types.StepStatusAwaitingApproval, 30*time.Second)
				if out, err := h.Run("axi", "status"); err != nil {
					t.Fatalf("parked status: %v\n%s", err, out)
				} else {
					t.Logf("parked CLI RESULT:\n%s", out)
				}
				if tc.recovered {
					if out, err := h.Run("daemon", "restart", "--force"); err != nil {
						t.Fatalf("recover approval: %v\n%s", err, out)
					}
					resumed := waitForStepStatus(t, h, branch, gate, types.StepStatusAwaitingApproval, 30*time.Second)
					if resumed.ID != parked.ID {
						t.Fatal("recovery replaced the parked run")
					}
				}
				deadline := time.Now().Add(15 * time.Second)
				for {
					if err := h.RespondError(parked.ID, gate, types.ActionApprove); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("custom gate did not accept ordinary approval")
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			run := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusRunning, 30*time.Second)
			database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			deadline := time.Now().Add(15 * time.Second)
			var publications []db.CommitStatusPublication
			var stored *db.Run
			for {
				stored, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				publications, err = database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				successRequested := false
				for _, state := range states {
					successRequested = successRequested || state == "success"
				}
				mu.Unlock()
				if stored.CIReadyAt != nil && (tc.recovered || successRequested && len(publications) == 1 && (tc.uncertain || publications[0].LastState == "success")) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("watcher never ready: %+v %+v", stored, publications)
				}
				time.Sleep(50 * time.Millisecond)
			}
			wantCertificate := tc.eligible || tc.dirty || tc.changedHead || tc.repair
			gotCertificate := false
			for _, p := range publications {
				gotCertificate = gotCertificate || p.StrictCertificate != nil
			}
			if gotCertificate != wantCertificate {
				t.Fatalf("executed gates certificate=%v, want %v", gotCertificate, wantCertificate)
			}
			if tc.mode != "legacy" {
				rows, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.StepName != gate {
						continue
					}
					found = true
					if row.Status != types.StepStatusCompleted || row.ExitCode == nil || *row.ExitCode != 0 || row.FindingsJSON == nil || row.OverrideReason != nil || row.ApprovalReason != nil {
						t.Fatalf("ordinary zero-exit approval changed: %+v", row)
					}
				}
				if !found {
					t.Fatal("custom gate was not executed")
				}
			}
			if tc.dirty || tc.changedHead {
				if err := os.WriteFile(filepath.Join(stored.WorktreePath(), "feature-drift.txt"), []byte("unvalidated work\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if tc.changedHead {
					for _, args := range [][]string{{"add", "feature-drift.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "unvalidated head"}} {
						if out, err := h.runGit(t.Context(), stored.WorktreePath(), args...); err != nil {
							t.Fatalf("git %v: %v\n%s", args, err, out)
						}
					}
				}
			}
			if tc.repair {
				beforeEpoch, err := database.StatusValidationEpoch(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				j.setForge(forgeState{Checks: []forgeCheck{redCheck("build")}})
				parked := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusAwaitingApproval, 45*time.Second)
				ci, ok := findStep(parked.Steps, types.StepCI)
				if !ok || ci.FindingsJSON == nil {
					t.Fatal("CI gate omitted its findings")
				}
				findings, err := types.ParseFindingsJSON(*ci.FindingsJSON)
				if err != nil || len(findings.Items) == 0 {
					t.Fatalf("CI findings: %+v %v", findings, err)
				}
				var ids []string
				for _, finding := range findings.Items {
					ids = append(ids, finding.ID)
				}
				h.RespondWithFindings(run.ID, types.StepCI, types.ActionFix, ids)
				j.waitCILog(run.ID, "CI fixer concluded no code change is needed", 15*time.Second)
				waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusFixReview, 15*time.Second)
				afterEpoch, err := database.StatusValidationEpoch(run.ID)
				if err != nil || afterEpoch <= beforeEpoch {
					t.Fatalf("repair did not invalidate validation generation: %d -> %d (%v)", beforeEpoch, afterEpoch, err)
				}
				stored, err := database.GetRun(run.ID)
				if err != nil || stored.HeadSHA != head {
					t.Fatalf("no-op repair changed head: %+v %v", stored, err)
				}
				publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil || len(publications) != 1 || publications[0].StrictCertificate != nil {
					t.Fatalf("no-op repair retained certificate: %+v %v", publications, err)
				}
				t.Logf("RESULT: no-op CI repair kept head %s and invalidated validation epoch %d -> %d", head, beforeEpoch, afterEpoch)
			}
			if out, err := h.Run("axi", "abort"); err != nil {
				t.Fatalf("abort: %v\n%s", err, out)
			} else {
				t.Logf("abort CLI RESULT:\n%s", out)
			}
			terminal := h.WaitForRun(branch, 15*time.Second)
			if terminal.Status != types.RunCancelled || !strings.Contains(deref(terminal.Error), "aborted by user") {
				t.Fatalf("untruthful cancellation: status=%s error=%q", terminal.Status, deref(terminal.Error))
			}
			deadline = time.Now().Add(10 * time.Second)
			for {
				publications, err = database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil || len(publications) != 1 && !tc.recovered {
					t.Fatalf("terminal publications: %+v %v", publications, err)
				}
				if tc.eligible || len(publications) == 0 || publications[0].LastState == "failure" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("ineligible cancellation did not post failure: %+v", publications[0])
				}
				time.Sleep(50 * time.Millisecond)
			}
			preserved := false
			for _, p := range publications {
				preserved = preserved || p.PreserveSuccess != nil && *p.PreserveSuccess
				if tc.eligible && p.LastState != "success" {
					t.Fatalf("eligible cancellation changed SUCCESS: %+v", p)
				}
				if !tc.eligible && p.StrictCertificate != nil {
					t.Fatalf("ineligible cancellation retained certificate: %+v", p)
				}
			}
			if preserved != tc.eligible {
				t.Fatalf("terminal preservation=%v, eligible=%v", preserved, tc.eligible)
			}
			mu.Lock()
			if tc.eligible && strings.Join(states, ",") != "pending,success" {
				t.Errorf("eligible cancellation posted additional statuses: %v", states)
			}
			t.Logf("RESULT: mode=%s recovered=%t head=%s cancelled=%s certificate_before=%t preserved=%t HTTP_states=%v", tc.mode, tc.recovered, head, terminal.Status, wantCertificate, preserved, states)
			mu.Unlock()
		})
	}
}
