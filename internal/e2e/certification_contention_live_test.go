//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the shared storage contract: a six-second writer must not
// turn a completed gate into a code failure; exhaustion must retain its typed
// infrastructure cause and must never publish a source-failure status. The
// Git wrapper only schedules a competing real SQLite writer at the completed
// gate's HEAD observation; every Git command still executes the real binary.
// External agent/forge responses remain fixtures, not live service evidence.
func TestCertificationContentionProductionJourney(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"release-after-six-seconds", "exhaust-budget"} {
		t.Run(mode, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: certifiedWatcherScenario(t)})
			h := j.h
			j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					State string `json:"state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
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
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\ngates:\n  - name: contention\n    after: rebase\n    command: 'true'\n")
			gate := types.CustomGateStepName(types.StepRebase, "contention")
			marker := filepath.Join(h.NMHome, "proof-head-observed")
			held := filepath.Join(h.NMHome, "proof-lock-held")
			wrapper := fmt.Sprintf(`#!%s
import os, sqlite3, sys, time
if sys.argv[1:] == ['rev-parse', 'HEAD'] and not os.path.exists(%q):
    try:
        c = sqlite3.connect(%q, timeout=1)
        row = c.execute("SELECT count(*) FROM step_results s WHERE s.step_name=? AND s.status='completed' AND NOT EXISTS(SELECT 1 FROM status_validation_steps v WHERE v.step_id=s.id)", (%q,)).fetchone()
        c.close()
        if row[0] and not os.path.exists(%q):
            fd = os.open(%q, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
            os.close(fd)
            deadline = time.monotonic()+20
            while not os.path.exists(%q):
                if time.monotonic()>deadline:
                    sys.exit('test writer handshake timed out')
                time.sleep(0.01)
    except sqlite3.Error:
        pass
os.execv(%q, [%q]+sys.argv[1:])
`, python, marker, filepath.Join(h.NMHome, "state.sqlite"), string(gate), marker, marker, held, realGit, realGit)
			if err := os.WriteFile(filepath.Join(h.BinDir, "git"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			j.init()
			branch := "feature/certification-contention-" + mode
			head := h.CommitChange(branch, "feature.txt", mode+"\n", "exercise proof storage contention")
			h.PushToGate(branch)
			deadline := time.Now().Add(45 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					h.dumpDebugState()
					t.Fatal("completed gate never reached its proof HEAD observation")
				}
				time.Sleep(20 * time.Millisecond)
			}
			control, err := sql.Open("sqlite", filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			control.SetMaxOpenConns(1)
			if _, err := control.Exec("BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer control.Exec("ROLLBACK")
			start := time.Now()
			if err := os.WriteFile(held, []byte("locked\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "exhaust-budget" {
				deadline = time.Now().Add(65 * time.Second)
				for {
					log, _ := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
					if strings.Contains(string(log), "SQLITE_WRITE_CONTENTION: infrastructure failure writing RecordStatusValidationStep") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("proof write never reported typed exhaustion")
					}
					time.Sleep(50 * time.Millisecond)
				}
			} else {
				<-time.After(6 * time.Second)
			}
			var proofCount int
			if err := control.QueryRow(`SELECT count(*) FROM status_validation_steps v JOIN step_results s ON s.id=v.step_id WHERE s.step_name=?`, gate).Scan(&proofCount); err != nil || proofCount != 0 {
				t.Fatalf("locked writer did not contain the proof write: count=%d err=%v", proofCount, err)
			}
			if _, err := control.Exec("ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if mode == "exhaust-budget" {
				run := h.WaitForRun(branch, 30*time.Second)
				if run.Status != types.RunFailed || !strings.HasPrefix(deref(run.Error), "SQLITE_WRITE_CONTENTION:") || !strings.Contains(deref(run.Error), "RecordStatusValidationStep") {
					t.Fatalf("lost infrastructure failure provenance: %+v", run)
				}
				publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range publications {
					if p.LastState == "failure" || p.StrictCertificate != nil {
						t.Fatalf("storage contention fabricated a source verdict: %+v", p)
					}
				}
				mu.Lock()
				posted := strings.Join(states, ",")
				mu.Unlock()
				if strings.Contains(posted, "failure") || strings.Contains(posted, "success") {
					t.Fatalf("contention published a source verdict: %s", posted)
				}
				t.Logf("RESULT: writer held %s; real daemon recorded failed with infrastructure cause %q; no failure status or certificate", time.Since(start), deref(run.Error))
				return
			}
			run := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusRunning, 45*time.Second)
			deadline = time.Now().Add(15 * time.Second)
			for {
				publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(publications) == 1 && publications[0].StrictCertificate != nil && publications[0].LastState == "success" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("six-second contention prevented certification: %+v", publications)
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Logf("RESULT: writer held beyond five-second busy timeout; real daemon recovered, completed actual gates and acknowledged SUCCESS at %s", head)
			t.Log(statusCommand(t, h, h.WorkDir, "axi", "status"))
		})
	}
}
