//go:build e2e && !windows

package e2e

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// Issue #158 and the owner's recovery decision are the independent oracle:
// no_ci plus an audited gap earns success; an ordinary exception does not;
// recovery cannot replace an accepted audit with a new approval reason.
// The CLI, daemon, Git delivery and HTTP status publisher are real. Only the
// external agent and forge are substituted. Recovery seeds the precise
// post-record/pre-completion crash state while the disposable daemon is off.
func TestNonStrictAcceptedGapNoCILiveJourney(t *testing.T) {
	for _, name := range []string{"accepted", "ordinary-exception", "recorded-recovery"} {
		t.Run(name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: acceptedTestGapScenario(t)})
			configureLocalGitHubSSH(t, j.h)
			j.setTrusted("no_ci: true\ncommands:\n  test: 'true'\n  lint: 'true'\nauto_fix:\n  test: 0\n")
			j.setForge(forgeState{Protection: protectionJSON()})
			posts := &journeyStatusServer{t: t}
			server := httptest.NewServer(posts)
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			branch := "feature/non-strict-no-ci-" + name
			j.push(branch)
			parked := waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			const reason = "  Owner approved via first mate; ticket #158  "
			const coveredBy = "  TestBrokerTimeoutMocked; broker/timeout_test.go  "
			refuse := func(label string, args ...string) string {
				t.Helper()
				out, err := j.h.Run(append([]string{"axi", "respond", "--step", "test"}, args...)...)
				if err == nil {
					t.Fatalf("%s was admitted:\n%s", label, out)
				}
				t.Logf("%s refused:\n%s", label, out)
				current := j.h.RunInfo(parked.ID)
				step, _ := findStep(current.Steps, types.StepTest)
				if step.Status != types.StepStatusAwaitingApproval {
					t.Fatalf("%s released the gate: %+v", label, step)
				}
				return out
			}
			if name == "accepted" {
				out, err := j.h.Run("axi", "status")
				if err != nil || !strings.Contains(out, "--accept-gap test-verdict") {
					t.Fatalf("plain Test gate did not offer gap acceptance: %v\n%s", err, out)
				}
				t.Logf("non-strict gate help:\n%s", out)
				for _, request := range []struct {
					name, ids, why, coverage string
				}{
					{"unknown finding", "missing", reason, coveredBy},
					{"short reason", "test-verdict", "ok", coveredBy},
					{"multiline reason", "test-verdict", "owner approved\nsecond line", coveredBy},
					{"empty coverage", "test-verdict", reason, "   "},
					{"multiline coverage", "test-verdict", reason, "TestX\nTestY"},
				} {
					refuse(request.name, "--action", "approve", "--accept-gap", request.ids, "--reason", request.why, "--covered-by", request.coverage)
				}
				refuse("gap on fix action", "--action", "fix", "--accept-gap", "test-verdict", "--reason", reason, "--covered-by", coveredBy)
			}
			var original string
			if name == "recorded-recovery" {
				if out, err := j.h.Run("daemon", "stop", "--force"); err != nil {
					t.Fatalf("stop isolated daemon: %v\n%s", err, out)
				}
				database, err := db.Open(filepath.Join(j.h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				rows, err := database.GetStepsByRun(parked.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.StepName == types.StepTest {
						if _, err := database.RecordAcceptedGaps(row.ID, db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}, false); err != nil {
							t.Fatal(err)
						}
						updated, err := database.GetStepResult(row.ID)
						if err != nil {
							t.Fatal(err)
						}
						original = *updated.AcceptedGapsJSON
					}
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
				if original == "" {
					t.Fatal("crash fixture has no recorded audit")
				}
				if out, err := j.h.Run("daemon", "start"); err != nil {
					t.Fatalf("restart isolated daemon: %v\n%s", err, out)
				}
				waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 30*time.Second)
				out := refuse("new reason over recovered audit", "--action", "approve", "--reason", "another approval reason")
				for _, want := range []string{"recorded audit is reused", "axi respond --action approve", "without --reason"} {
					if !strings.Contains(out, want) {
						t.Fatalf("recovery refusal omitted %q:\n%s", want, out)
					}
				}
			}
			args := []string{"axi", "respond", "--step", "test", "--action", "approve", "--wait", "90s"}
			if name != "recorded-recovery" {
				args = append(args, "--reason", reason)
			}
			if name == "accepted" {
				args = append(args, "--accept-gap", "test-verdict", "--covered-by", coveredBy)
			}
			out, err := j.h.Run(args...)
			if err != nil {
				t.Fatalf("approve: %v\n%s", err, out)
			}
			t.Logf("approval CLI:\n%s", out)
			ready := j.expectReady(branch, 90*time.Second)
			if ready.Shipping != nil || !ready.CIReadyNoCI {
				t.Fatalf("expected plain no_ci readiness: %+v", ready)
			}
			if j.h.UpstreamBranchSHA(branch) != ready.HeadSHA {
				t.Fatal("status does not name the delivered head")
			}
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			rows, err := database.GetStepsByRun(ready.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.StepName != types.StepTest {
					continue
				}
				if name == "ordinary-exception" {
					if row.AcceptedGapsJSON != nil || row.ApprovalReason == nil || *row.ApprovalReason != reason {
						t.Fatalf("plain approval became a gap: %+v", row)
					}
				} else {
					gap := row.ValidAcceptedGaps()
					if gap == nil || gap.Reason != reason || gap.CoveredBy != coveredBy || row.ApprovalReason != nil {
						t.Fatalf("audit lost or rewritten: %+v", row)
					}
					if original != "" && *row.AcceptedGapsJSON != original {
						t.Fatal("recovery rewrote the recorded audit")
					}
					t.Logf("durable audit: %s", *row.AcceptedGapsJSON)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for name != "ordinary-exception" && len(posts.history(ready.HeadSHA)) < 2 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if name == "ordinary-exception" {
				posts.assert(t, ready.HeadSHA, "pending")
			} else {
				posts.assert(t, ready.HeadSHA, "pending", "success")
				history := posts.history(ready.HeadSHA)
				if !strings.HasPrefix(history[1].Description, "passed with accepted test gap:") {
					t.Fatalf("success concealed accepted gap: %+v", history)
				}
			}
			t.Logf("HTTP status receipts: %+v", posts.history(ready.HeadSHA))
			j.finish(branch)
		})
	}
}

// These are real command failures and real agent-budget expiry, plus external
// agent responses containing a no-go verdict or an additional actionable
// finding. Naming a gap must never turn any of them into a successful Test.
func TestNonStrictAcceptedGapRefusesUnsafeLiveJourney(t *testing.T) {
	for _, name := range []string{"command", "no-go", "budget", "uncovered-finding"} {
		t.Run(name, func(t *testing.T) {
			scenario := acceptedTestGapScenario(t)
			data, err := os.ReadFile(scenario)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Actions []map[string]any `yaml:"actions"`
			}
			if err := yaml.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			for _, action := range document.Actions {
				if action["match"] != "You are validating a code change by driving the product itself." {
					continue
				}
				structured := action["structured"].(map[string]any)
				switch name {
				case "no-go":
					structured["verdict"] = "no-go"
					structured["scenarios"] = []map[string]any{{"name": "external agent reported failure", "result": "fail", "live": true, "evidence": "deliberate negative agent input", "reason": ""}}
				case "budget":
					action["delay_ms"] = 60000
				case "uncovered-finding":
					structured["findings"] = []map[string]any{{"id": "uncovered", "severity": "warning", "action": "ask-user", "description": "additional actionable issue"}}
				}
			}
			data, err = yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(scenario, data, 0o600); err != nil {
				t.Fatal(err)
			}
			opts := SetupOpts{Agent: "claude", Scenario: scenario}
			if name == "budget" {
				opts.GlobalConfigExtra = "test_agent_timeout: '1s'\ntest_agent_working_timeout: '1s'\n"
			}
			j := newScopeJourney(t, opts)
			command := "true"
			if name == "command" {
				command = "exit 7"
			}
			j.setTrusted("no_ci: true\ncommands:\n  test: '" + command + "'\n  lint: 'true'\nauto_fix:\n  test: 0\n")
			j.setForge(forgeState{Protection: protectionJSON()})
			j.init()
			branch := "feature/unsafe-gap-" + name
			j.push(branch)
			parked := waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			out, err := j.h.Run("axi", "respond", "--step", "test", "--action", "approve", "--accept-gap", "test-verdict", "--reason", "owner approved negative fixture", "--covered-by", "TestBrokerTimeoutMocked")
			if err == nil {
				t.Fatalf("unsafe %s gap was admitted:\n%s", name, out)
			}
			t.Logf("unsafe %s gap refused:\n%s", name, out)
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			rows, err := database.GetStepsByRun(parked.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.StepName == types.StepTest {
					if row.Status != types.StepStatusAwaitingApproval || row.AcceptedGapsJSON != nil || row.ApprovalReason != nil {
						t.Fatalf("refusal released or rewrote the gate: %+v", row)
					}
					findings, err := types.ParseFindingsJSON(deref(row.FindingsJSON))
					if err != nil {
						t.Fatal(err)
					}
					switch name {
					case "command":
						if row.ExitCode == nil || *row.ExitCode != 7 || !strings.Contains(out, "non-zero exit code") {
							t.Fatalf("did not exercise command failure: %+v\n%s", row, out)
						}
					case "no-go":
						if findings.Verdict != types.TestVerdictNoGo || !strings.Contains(out, "no-go") {
							t.Fatalf("did not exercise no-go refusal: %+v\n%s", findings, out)
						}
					case "budget":
						found := false
						for _, finding := range findings.Items {
							found = found || finding.ID == types.FindingIDTestAgentTimeout
						}
						if !found {
							t.Fatalf("did not exercise budget expiry: %+v", findings)
						}
					case "uncovered-finding":
						if !strings.Contains(out, "missing: uncovered") {
							t.Fatalf("did not exercise finding coverage: %s", out)
						}
					}
					t.Logf("parked Test findings: %s", deref(row.FindingsJSON))
				}
			}
			j.finish(branch)
		})
	}
}
