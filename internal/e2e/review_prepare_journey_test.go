//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Real CLI, daemon, Git worktree, setup shell, fix commit, and evidence storage;
// only the agent and forge are fixtures. The fixer executes the installed
// runner against its own edit. A canned "passed" reply alone cannot pass this
// journey: the wrapper refuses to emit it unless that real command succeeds.
func TestReviewDependencyPreparationJourney(t *testing.T) {
	allow := false
	h := NewHarness(t, SetupOpts{Agent: "claude", AllowRepoCommands: &allow})
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(h.HomeDir, "setup-calls")
	verified := filepath.Join(h.HomeDir, "fix-verified")
	evidence := filepath.Join(h.HomeDir, "evidence-ran")
	initial := filepath.Join(h.HomeDir, "initial.yaml")
	fix := filepath.Join(h.HomeDir, "fix.yaml")
	write(initial, `actions:
  - structured:
      findings:
        - id: dependency-bug
          severity: error
          file: feature.txt
          line: 1
          description: feature must say fixed
          action: auto-fix
      summary: repair feature
      risk_level: medium
      risk_rationale: broken feature
      risk_scope: source-or-external
`)
	write(fix, `actions:
  - structured:
      summary: repair feature
      verification: passed
`)
	delegate := filepath.Join(h.HomeDir, "delegate")
	if err := os.Mkdir(delegate, 0o755); err != nil {
		t.Fatal(err)
	}
	realAgent := filepath.Join(delegate, "claude")
	if err := os.Symlink(h.FakeAgent, realAgent); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.BinDir, "claude")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(h.BinDir, "claude"), fmt.Sprintf(`#!/bin/sh
set -eu
prompt=$(cat)
scenario=''
case "$prompt" in
 *'Investigate previous review findings and address legitimate ones.'*)
   if [ ! -x node_modules/.bin/tsx ]; then
     echo 'verification blocked by missing tsx' >&2
     exit 44
   fi
   echo fixed > feature.txt
   node_modules/.bin/tsx
   test -z "$(git diff --name-only -- . ':!feature.txt')"
   test ! -e prepare-stray
   echo verified >> %s
   scenario=%s ;;
 *'Review the code changes and return structured findings'*)
   if [ ! -f %s ]; then
     test ! -d node_modules
     scenario=%s
   elif [ "$(wc -l < %s)" -eq 1 ]; then
     scenario=%s
   fi ;;
 *'You are validating a code change by driving the product itself'*)
   node_modules/.bin/tsx
   echo evidence >> %s ;;
esac
if [ -n "$scenario" ]; then
 printf '%%s' "$prompt" | FAKEAGENT_SCENARIO="$scenario" %s "$@"
else
 printf '%%s' "$prompt" | %s "$@"
fi
`, shellQuote(verified), shellQuote(fix), shellQuote(verified), shellQuote(initial), shellQuote(verified), shellQuote(initial), shellQuote(evidence), shellQuote(realAgent), shellQuote(realAgent)))
	runner := filepath.Join(h.BinDir, "fixture-tsx")
	write(runner, "#!/bin/sh\nset -eu\ntest \"$(cat feature.txt)\" = fixed\n")
	write(filepath.Join(h.BinDir, "nm-prepare"), fmt.Sprintf("#!/bin/sh\nset -eu\nmkdir -p node_modules/.bin\ncp %s node_modules/.bin/tsx\necho install >> %s\necho mutation > README.md\necho stray > prepare-stray\n", shellQuote(runner), shellQuote(calls)))
	h.CommitChange("main", ".gitignore", "node_modules/\n", "ignore dependencies")
	pushMainRepoConfig(t, h, "allow_repo_commands: false\ncommands:\n  prepare: nm-prepare\n  test: node_modules/.bin/tsx\n  lint: node_modules/.bin/tsx\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/review-dependencies"
	submitted := h.CommitChange(branch, "feature.txt", "broken\n", "add feature")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("read-only review paid setup: %v", err)
	}
	h.RespondWithFindings(parked.ID, types.StepReview, types.ActionFix, []string{"dependency-bug"})
	first, _ := findStep(parked.Steps, types.StepReview)
	second := documentJourneyNextGate(t, h, parked.ID, types.StepReview, first.Gate.Nonce)
	if out, err := h.Run("daemon", "stop", "--force"); err != nil {
		t.Fatalf("stop between fix rounds: %v\n%s", err, out)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("recover prepared worktree: %v\n%s", err, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := h.respondError(second.ID, types.StepReview, types.ActionFix, []string{"dependency-bug"}); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("recovered gate refused second fix: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("status=%s error=%s\n%s", run.Status, deref(run.Error), readStepLog(t, h, run.ID, "review"))
	}
	for _, path := range []string{verified, evidence, calls} {
		data, err := os.ReadFile(path)
		want := 1
		if path == verified {
			want = 2
		}
		if err != nil || len(strings.Fields(string(data))) != want {
			t.Fatalf("receipt %s=%q error=%v, want %d executions", path, data, err, want)
		}
		t.Logf("execution receipt %s: %s", filepath.Base(path), data)
	}
	if got := h.UpstreamBranchSHA(branch); got != run.HeadSHA {
		t.Fatalf("published head=%s, want validated %s", got, run.HeadSHA)
	}
	step, ok := findStep(run.Steps, types.StepTest)
	if !ok || step.Status != types.StepStatusCompleted || step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, `"verdict":"go"`) {
		t.Fatalf("Test evidence lost: %+v", step)
	}
	if got := gitIn(t, h, h.UpstreamDir, "diff", "--name-only", submitted, run.HeadSHA); strings.TrimSpace(got) != "feature.txt" {
		t.Fatalf("setup artifacts in fix: %s", got)
	}
	if got := gitIn(t, h, h.UpstreamDir, "rev-list", "--count", submitted+".."+run.HeadSHA); strings.TrimSpace(got) != "1" {
		t.Fatalf("extra setup/format commit: %s", got)
	}
	if out, err := h.Run("axi", "status", "--run", run.ID); err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	} else {
		t.Logf("actual CLI status:\n%s", out)
	}
	t.Logf("actual review log:\n%s", readStepLog(t, h, run.ID, "review"))
	t.Logf("run=%s submitted=%s head=%s; two fixer checks across daemon recovery; setup once; Test evidence persisted; one substantive commit", run.ID, submitted, run.HeadSHA)
}
