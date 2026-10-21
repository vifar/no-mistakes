//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The issue #77/#79 oracle is the executable present at agent entry, not
// the simulated agent's success claim. Real CLI, daemon, shell and Git own
// preparation, dispatch and commit accounting; only agent replies are canned.
func TestEditingDependencyPreparationJourney(t *testing.T) {
	for _, tc := range []struct {
		name         string
		consumer     string
		fail         bool
		noPrepare    bool
		baselineFail bool
	}{
		{name: "document", consumer: "document"},
		{name: "combined_lint", consumer: "combined"},
		{name: "standalone_lint", consumer: "lint"},
		{name: "no_prepare", consumer: "combined", noPrepare: true},
		{name: "configured_test", consumer: "test"},
		{name: "failed_test_still_gets_evidence", consumer: "test", baselineFail: true},
		{name: "document_setup_failure", consumer: "document", fail: true},
		{name: "lint_setup_failure", consumer: "lint", fail: true},
		{name: "review_setup_failure", consumer: "review", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow := false
			h := NewHarness(t, SetupOpts{Agent: "claude", AllowRepoCommands: &allow})
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			delegateDir := filepath.Join(h.HomeDir, "delegate")
			if err := os.Mkdir(delegateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			delegate := filepath.Join(delegateDir, "claude")
			if err := os.Symlink(h.FakeAgent, delegate); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(h.BinDir, "claude")); err != nil {
				t.Fatal(err)
			}
			check := "test -x node_modules/.bin/tsx\nnode_modules/.bin/tsx\n"
			if tc.noPrepare {
				check = "test ! -e node_modules\n"
			}
			testCheck := "test ! -e node_modules"
			if tc.consumer == "test" {
				testCheck = "test -f \"$NM_HOME/baseline-entry\"\n" + check
			}
			write(filepath.Join(h.BinDir, "claude"), fmt.Sprintf(`#!/bin/sh
set -eu
prompt=$(cat)
purpose=''
case "$prompt" in
 *'Investigate previous review findings and address legitimate ones.'*) purpose=review ;;
 *'Find what this change made stale'*) purpose=document ;;
 *'Detect the linting and formatting tools for this project'*) purpose=lint ;;
 *'You are validating a code change by driving the product itself'*)
   %s
   echo evidence >> "$NM_HOME/evidence-entry" ;;
 *'Review the code changes and return structured findings'*) test ! -e node_modules ;;
esac
if [ -n "$purpose" ]; then
 %s
 test "$(cat README.md)" = '# e2e'
 test ! -e prepare-stray
 echo "$purpose:$(pwd)" >> "$NM_HOME/editing-entry"
fi
printf '%%s' "$prompt" | %s "$@"
`, testCheck, check, shellQuote(delegate)))
			write(filepath.Join(h.BinDir, "fixture-runner"), "#!/bin/sh\nset -eu\necho fixture-runner-executed\n")
			prepare := "#!/bin/sh\nset -eu\necho attempt >> \"$NM_HOME/setup-attempts\"\nmkdir -p node_modules/.bin\ncp \"$(command -v fixture-runner)\" node_modules/.bin/tsx\necho mutation > README.md\necho stray > prepare-stray\n"
			if tc.fail {
				prepare += "echo deliberate-setup-failure >&2\nexit 23\n"
			}
			write(filepath.Join(h.BinDir, "nm-prepare"), prepare)
			h.CommitChange("main", ".gitignore", "node_modules/\n", "ignore dependencies")
			cfg := "allow_repo_commands: false\nignore_patterns: ['*.generated.go']\ncommands:\n"
			if !tc.noPrepare {
				cfg += "  prepare: nm-prepare\n"
			}
			if tc.consumer == "test" {
				cfg += "  test: 'node_modules/.bin/tsx && echo baseline >> \"$NM_HOME/baseline-entry\""
				if tc.baselineFail {
					cfg += "; exit 7"
				}
				cfg += "'\n  lint: 'true'\n"
			} else if tc.consumer == "document" || tc.noPrepare {
				cfg += "  lint: 'true'\n"
			} else {
				cfg += "  test: ''\n"
			}
			if tc.consumer == "review" {
				h.Scenario = writeReviewAgentsRoutingScenario(t)
				t.Setenv("FAKEAGENT_SCENARIO", h.Scenario)
			}
			pushMainRepoConfig(t, h, cfg)
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/prepare-" + tc.name
			path := "feature.txt"
			if tc.consumer == "lint" {
				path = "feature.generated.go" // Document's normal no-change off-ramp.
			}
			submitted := h.CommitChange(branch, path, "fixture change\n", "add fixture change")
			h.PushToGate(branch)
			if tc.consumer == "review" {
				parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
				h.RespondWithFindings(parked.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
			}
			var run *ipc.RunInfo
			if tc.baselineFail {
				run = waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			} else {
				run = h.WaitForRun(branch, 90*time.Second)
			}
			entry, entryErr := os.ReadFile(filepath.Join(h.NMHome, "editing-entry"))
			if tc.baselineFail {
				step, ok := findStep(run.Steps, types.StepTest)
				if !ok || step.ExitCode == nil || *step.ExitCode != 7 || step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, `"verdict":"go"`) {
					t.Fatalf("baseline failure or evidence was lost: %+v", step)
				}
				if !os.IsNotExist(entryErr) {
					t.Fatalf("later editing agent entered at failing Test gate: %q (%v)", entry, entryErr)
				}
			} else if tc.fail {
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "prepare command exited with code 23") {
					t.Fatalf("expected explicit setup failure: %+v", run)
				}
				if !os.IsNotExist(entryErr) {
					t.Fatalf("editing agent entered after failed setup: %q (%v)", entry, entryErr)
				}
			} else {
				if run.Status != types.RunCompleted || entryErr != nil || len(strings.Fields(string(entry))) != 1 {
					t.Fatalf("run=%s error=%v editing entries=%q (%v)", run.Status, run.Error, entry, entryErr)
				}
				want := "document:"
				if tc.consumer == "lint" {
					want = "lint:"
				}
				if !strings.HasPrefix(string(entry), want) {
					t.Fatalf("wrong agent entered: %q", entry)
				}
				if got := h.UpstreamBranchSHA(branch); got != submitted || run.HeadSHA != submitted {
					t.Fatalf("setup created a commit: submitted=%s published=%s run=%s", submitted, got, run.HeadSHA)
				}
			}
			attempts, err := os.ReadFile(filepath.Join(h.NMHome, "setup-attempts"))
			if tc.noPrepare {
				if !os.IsNotExist(err) {
					t.Fatalf("undeclared preparation ran: %q (%v)", attempts, err)
				}
			} else if err != nil || string(attempts) != "attempt\n" {
				t.Fatalf("setup attempts=%q (%v), want one", attempts, err)
			}
			if tc.consumer == "test" {
				data, err := os.ReadFile(filepath.Join(h.NMHome, "evidence-entry"))
				if err != nil || string(data) != "evidence\n" {
					t.Fatalf("Test did not perform its unconditional evidence turn: %q (%v)", data, err)
				}
				t.Logf("actual Test log:\n%s", readStepLog(t, h, run.ID, "test"))
			}
			if out, err := h.Run("axi", "status", "--run", run.ID); err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			} else {
				t.Logf("actual CLI status:\n%s", out)
			}
			t.Logf("setup receipt=%q; editing entry=%q; submitted=%s final=%s", attempts, entry, submitted, run.HeadSHA)
		})
	}
}
