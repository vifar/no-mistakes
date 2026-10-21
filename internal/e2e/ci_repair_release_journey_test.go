//go:build e2e && !windows

package e2e

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A continuity-proven repair may finish at checks-passed without revalidation.
// Both readiness paths must follow the published head and report success for it.
func TestCIRepairReleaseJourney(t *testing.T) {
	for _, noCI := range []bool{false, true} {
		t.Run(fmt.Sprint(noCI), func(t *testing.T) {
			branch := fmt.Sprintf("feature/repaired-%v", noCI)
			basename := "scope-" + strings.ReplaceAll(branch, "/", "-") + ".txt"
			scenario := certifiedWatcherScenario(t)
			data, err := os.ReadFile(scenario)
			if err != nil {
				t.Fatal(err)
			}
			repair := fmt.Sprintf("actions:\n  - match: '- failing checks: build'\n    edits:\n      - path: %s\n        new: 'CI repair effect'\n    structured:\n      summary: repaired build\n      code_change_needed: true\n", basename)
			if err := os.WriteFile(scenario, []byte(strings.Replace(string(data), "actions:\n", repair, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "ci_readiness_receipt_timeout: 1s\n"})
			configureLocalGitHubSSH(t, j.h)
			cfg := "commands:\n  test: 'true'\n  lint: 'true'\nci:\n  release_after_checks_pass: true\n  revalidate_repairs: false\n  external_checks: [no-mistakes]\n"
			if noCI {
				cfg += "no_ci: true\n"
			}
			j.setTrusted(cfg)
			j.setForge(forgeState{Protection: protectionJSON(), Checks: []forgeCheck{redCheck("build")}})
			posts := &journeyStatusServer{t: t}
			server := httptest.NewServer(posts)
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			j.push(branch)
			defer j.finish(branch)
			run, _ := j.expectGate(branch)
			ci, _ := findStep(run.Steps, types.StepCI)
			findings, err := types.ParseFindingsJSON(deref(ci.FindingsJSON))
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("repair gate: %+v, %v", findings, err)
			}
			out, err := j.h.Run("axi", "respond", "--step", "ci", "--action", "fix", "--findings", findings.Items[0].ID, "--wait", "1s")
			if err != nil && !strings.Contains(out, "wait of 1s elapsed") {
				t.Fatalf("dispatch repair: %v\n%s", err, out)
			}
			deadline := time.Now().Add(90 * time.Second)
			newHead := run.HeadSHA
			for time.Now().Before(deadline) {
				newHead = j.h.UpstreamBranchSHA(branch)
				if newHead != run.HeadSHA {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if newHead == run.HeadSHA {
				t.Fatalf("repair never published:\n%s", j.ciLog(run.ID))
			}
			checks := []forgeCheck{greenCheck("build")}
			if noCI {
				checks = nil
			}
			j.setForge(forgeState{Protection: protectionJSON(), Checks: checks})
			deadline = time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				current := j.h.RunInfo(run.ID)
				if current.Status.Terminal() {
					break
				}
				if strings.Contains(j.ciLog(run.ID), "warning: CI release comparison could not be verified") {
					t.Fatalf("published repair cannot release:\n%s", j.ciLog(run.ID))
				}
				time.Sleep(100 * time.Millisecond)
			}
			final := j.h.WaitForRun(branch, 10*time.Second)
			if final.ID != run.ID || final.Status != types.RunChecksPassed || final.HeadSHA != newHead || !final.CIReady || final.CIReadyNoCI != noCI {
				t.Fatalf("repaired head not validated: %+v\n%s", final, j.ciLog(run.ID))
			}
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			receipt, err := database.GetRunPRContext(run.ID)
			if err != nil || receipt == nil || receipt.LocalHeadSHA != newHead || receipt.ForgeHeadSHA != newHead {
				t.Fatalf("published comparison = %+v, %v; want repaired head %s", receipt, err, newHead)
			}
			deadline = time.Now().Add(10 * time.Second)
			for len(posts.history(newHead)) < 2 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			posts.assert(t, newHead, "pending", "success")
		})
	}
}
