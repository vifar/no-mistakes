//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/toon-format/toon-go"
)

// Issue #122 is the oracle: a trusted no_ci declaration must reach the
// agent's checks-passed outcome and publish success at the delivered head,
// while a real pending check must keep waiting. Exercise the CLI, daemon,
// config trust selection, run row and status HTTP boundary together.
func TestNoCIReadinessJourney(t *testing.T) {
	for _, name := range []string{"declared", "declared-self-status", "undeclared", "real-pending", "real-green"} {
		t.Run(name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: certifiedWatcherScenario(t)})
			configureLocalGitHubSSH(t, j.h)
			declared := name != "undeclared"
			extra := "commands:\n  test: 'true'\n  lint: 'true'\nci:\n  external_checks: [no-mistakes]\n"
			if declared {
				extra += "no_ci: true\n"
			}
			if name == "declared-self-status" {
				extra = strings.Replace(extra, "  external_checks: [no-mistakes]", "  external_checks: []", 1)
			}
			j.setTrusted(extra)
			if _, err := os.Stat(filepath.Join(j.h.WorkDir, ".github", "workflows")); !os.IsNotExist(err) {
				t.Fatalf("no-CI fixture must have no workflows: %v", err)
			}
			forge := forgeState{Protection: protectionJSON(), Statuses: []forgeStatus{{Context: "no-mistakes", State: "PENDING"}}}
			switch name {
			case "declared-self-status":
				forge.Protection = protectionJSON("no-mistakes")
			case "real-pending":
				forge.Checks = []forgeCheck{runningCheck("build")}
			case "real-green":
				forge.Checks = []forgeCheck{greenCheck("build")}
			}
			j.setForge(forge)
			posts := &journeyStatusServer{t: t}
			server := httptest.NewServer(posts)
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			branch := "feature/no-ci-" + name
			j.push(branch)
			defer j.finish(branch)
			ready := name != "undeclared" && name != "real-pending"
			noCI := ready && name != "real-green"
			var runID string
			if ready {
				runID = j.expectReady(branch, scopeCheckWaitTime).ID
			} else {
				waiting := "no CI checks reported yet, waiting for checks to register..."
				if name == "real-pending" {
					waiting = "CI checks running, waiting for results..."
				}
				runID = j.expectWaiting(branch, waiting).ID
				if name == "undeclared" && !strings.Contains(j.ciLog(runID), "declare no_ci: true in .no-mistakes.yaml on the trusted default branch") {
					t.Error("empty-check wait did not explain the existing deliberate no-CI remedy")
				}
			}
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			run, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			if (run.CIReadyAt != nil) != ready || run.CIReadyNoCI != noCI {
				t.Errorf("persisted readiness: at=%v no_ci=%v; want ready=%v no_ci=%v", run.CIReadyAt, run.CIReadyNoCI, ready, noCI)
			}
			out, err := j.h.Run("axi", "status", "--run", runID)
			if err != nil {
				t.Fatalf("axi status: %v\n%s", err, out)
			}
			var status struct {
				Run struct {
					Ready *bool `toon:"ci_ready"`
					NoCI  *bool `toon:"ci_ready_no_ci"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(out, &status); err != nil {
				t.Fatal(err)
			}
			if status.Run.Ready == nil || status.Run.NoCI == nil || *status.Run.Ready != ready || *status.Run.NoCI != noCI {
				t.Errorf("ordinary axi status lost readiness/reason:\n%s", out)
			}
			if ready {
				out, err := j.h.Run("axi", "run", "--intent", "Validate issue 122 no-CI readiness", "--wait", "5s")
				if err != nil || !strings.Contains(out, "outcome: checks-passed") {
					t.Errorf("no terminal agent-facing readiness outcome: %v\n%s", err, out)
				}
				deadline := time.Now().Add(10 * time.Second)
				for len(posts.history(run.HeadSHA)) < 2 && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
				posts.assert(t, run.HeadSHA, "pending", "success")
				if j.h.UpstreamBranchSHA(branch) != run.HeadSHA {
					t.Fatal("success status does not name the exact published head")
				}
			}
			evidence, _ := json.Marshal(struct{ Ready, NoCI bool }{run.CIReadyAt != nil, run.CIReadyNoCI})
			t.Logf("run row %s: head=%s readiness=%s", runID, run.HeadSHA, evidence)
		})
	}
}
