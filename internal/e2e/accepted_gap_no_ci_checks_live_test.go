//go:build e2e && !windows

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #158 explicitly preserves the no_ci contract: an accepted local Test
// gap must not waive a pending or failing in-scope forge check. Drive the real
// CLI and daemon with an external forge fixture, and observe CI readiness and
// the published audit. Local validation success is independent of external CI.
func TestNonStrictAcceptedGapNoCINeverWaivesLiveChecks(t *testing.T) {
	for _, name := range []string{"pending", "failing"} {
		t.Run(name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: acceptedTestGapScenario(t)})
			configureLocalGitHubSSH(t, j.h)
			j.setTrusted("no_ci: true\ncommands:\n  test: 'true'\n  lint: 'true'\nauto_fix:\n  test: 0\n")
			check := runningCheck("build")
			if name == "failing" {
				check = redCheck("build")
			}
			j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{check}})
			j.init()
			branch := "feature/gap-no-ci-" + name
			j.push(branch)
			waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			out, err := j.h.Run("axi", "respond", "--step", "test", "--action", "approve", "--accept-gap", "test-verdict", "--reason", "owner approved isolated broker gap", "--covered-by", "TestBrokerTimeoutMocked", "--wait", "1s")
			if err != nil && !strings.Contains(out, "wait of 1s elapsed") {
				t.Fatalf("accept local gap: %v\n%s", err, out)
			}
			t.Logf("local Test gap accepted:\n%s", out)
			if name == "pending" {
				j.expectWaiting(branch, "CI checks running, waiting for results...")
			} else {
				_, checks := j.expectGate(branch)
				if !sameSet(checks, "build") {
					t.Fatalf("CI gate names %v, want failing build", checks)
				}
			}
			body := livePRBody(t, j.statePath, branch)
			if !strings.Contains(body, "### Accepted gaps") || !strings.Contains(body, "owner approved isolated broker gap") || !strings.Contains(body, "TestBrokerTimeoutMocked") {
				t.Fatalf("published PR lost the local gap audit:\n%s", body)
			}
			t.Logf("published PR body:\n%s", body)
			j.finish(branch)
		})
	}
}
