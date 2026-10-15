//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Replay carry cannot bypass the fork's pinned PR comparison. Moving the
// target after review must fail closed instead of authorizing a changed
// comparison, even with both upstream replay and behind-repair options on.
func TestCIReplayOptionCannotBypassMovedComparisonOnRealBinary(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "ci-replay.yaml")
	content := `actions:
  - match: "The PR has no merge conflicts, but it is behind a base branch"
    git:
      - ["fetch", "origin", "main"]
      - ["rebase", "origin/main"]
    structured:
      summary: "replay onto the advanced base"
      code_change_needed: true
  - text: "clean"
    structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "fixture has no defects"
      risk_scope: source-or-external
      tested: ["fixture check"]
      testing_summary: "fixture check passed"
      artifacts: []
      scenarios:
        - {name: "fixture check", result: pass, live: true, evidence: "scripted input", reason: ""}
      verdict: go
      title: "feat: replay comparison probe"
      body: "Replay comparison probe"
`
	if err := os.WriteFile(scenario, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          scenario,
		GlobalConfigExtra: "ci:\n  rebase_behind: true\n  carry_review_on_replay: true\nci_timeout: 180s\n",
	})
	setupStatefulGitHub(t, h)
	const branch = "feature/ci-replay-comparison"
	marker := filepath.Join(h.HomeDir, "behind-head")
	delegateDir := filepath.Join(h.BinDir, "delegate")
	if err := os.MkdirAll(delegateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	delegate := filepath.Join(delegateDir, "gh")
	if err := os.Symlink(h.FakeAgent, delegate); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(h.BinDir, "gh")
	if err := os.Remove(gh); err != nil {
		t.Fatal(err)
	}
	checks := `{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"fixture-build","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"slug":"fixture-ci"}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}}`
	script := "#!/bin/sh\ncase \"$*\" in\n*'statusCheckRollup'*) printf '%s\\n' " + shellQuote(checks) + " ; exit 0 ;;\n*'mergeable,mergeStateStatus'*)\n" +
		"if [ -f " + shellQuote(marker) + " ] && [ \"$(git --git-dir=" + shellQuote(h.UpstreamDir) + " rev-parse refs/heads/" + branch + ")\" = \"$(cat " + shellQuote(marker) + ")\" ]; then printf 'MERGEABLE BEHIND\\n'; else printf 'MERGEABLE CLEAN\\n'; fi\nexit 0 ;;\nesac\nexec " + shellQuote(delegate) + " \"$@\"\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	head := h.CommitChange(branch, "feature.txt", "reviewed feature\n", "add reviewed feature")
	h.PushToGate(branch)
	monitoring := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusRunning, 90*time.Second)
	if out, err := h.runGit(t.Context(), h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(h.WorkDir, "base-advance.txt"), []byte("independent base advance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "base-advance.txt"}, {"commit", "-m", "advance base"}, {"push", "origin", "main"}} {
		if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(marker, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	run := h.WaitForRun(branch, 150*time.Second)
	if run.ID != monitoring.ID || run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "PR comparison changed after rebase") || run.HeadSHA != head {
		t.Fatalf("moved comparison was not refused: status=%s head=%s error=%s", run.Status, run.HeadSHA, deref(run.Error))
	}
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(run.ID)
	if err != nil || stored == nil || stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != head {
		t.Fatalf("refusal changed the original review authority: %+v, %v", stored, err)
	}
	pending, err := database.RunTestRestartPending(run.ID)
	if err != nil || pending {
		t.Fatalf("refused comparison acquired replay carry: pending=%v error=%v", pending, err)
	}
	t.Logf("replay and behind-repair options enabled; moved comparison refused: %s; reviewed head retained=%s; pending replay carry=false", *run.Error, *stored.ReviewApprovedHeadSHA)
}
