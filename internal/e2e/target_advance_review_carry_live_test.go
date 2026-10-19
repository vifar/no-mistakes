//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the required outstanding-finding rule: fixing A and returning
// an empty rereview cannot decide an unselected ask-user finding B. The real
// CLI and daemon must archive B at the rewind boundary and review again; the
// genuinely clean control may reuse review credit. Only agent/forge inputs are
// simulated, and no response approves or declines B.
func TestTargetAdvanceReviewCarryLiveJourney(t *testing.T) {
	for _, outstanding := range []bool{false, true} {
		t.Run(fmt.Sprintf("outstanding=%t", outstanding), func(t *testing.T) {
			scenario := cleanReviewScenario(t)
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			commitTrustedRepoConfig(t, h, "auto_fix:\n  review: 1\n")
			base := h.UpstreamBranchSHA("main")
			target := h.CommitChange("main", "landed.txt", "landed target\n", "prepare target")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/review-carry-advance"
			head := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
			old, err := os.ReadFile(scenario)
			if err != nil {
				t.Fatal(err)
			}
			additional := ""
			if outstanding {
				additional = `        - id: b
          severity: warning
          file: feature.txt
          line: 1
          description: "Operator decision remains open"
          action: ask-user
`
			}
			actions := fmt.Sprintf(`actions:
  - match: "Investigate previous review findings"
    edits:
      - path: review-fix.txt
        new: "review fix survives\n"
    git:
      - ["--git-dir=%s", update-ref, refs/heads/main, %s]
    structured:
      findings: []
      summary: "fixed A"
  - match: "- review scope: current worktree and HEAD changes relative to base commit"
    structured:
      findings: []
      summary: "raw rereview is empty"
      risk_level: low
      risk_rationale: "A was repaired"
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: a
          severity: warning
          file: feature.txt
          line: 1
          description: "Add the missing review fix"
          action: auto-fix
%s      summary: "initial findings"
      risk_level: medium
      risk_rationale: "fixture requires review"
      risk_scope: source-or-external
`, h.UpstreamDir, target, additional)
			if err := os.WriteFile(scenario, []byte(strings.Replace(string(old), "actions:\n", actions, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			initFromOwnWorktree(t, h, "init-review-carry")
			h.Checkout(branch)
			h.PushToGate(branch)
			if outstanding {
				parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 120*time.Second)
				database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				history, err := database.GetTargetRewindHistory(parked.ID)
				_ = database.Close()
				if err != nil || len(history) != 1 || history[0].BoundaryJSON == nil {
					t.Fatalf("missing rewind boundary: %+v %v", history, err)
				}
				var boundary pipeline.StepOutcome
				if err := json.Unmarshal([]byte(*history[0].BoundaryJSON), &boundary); err != nil {
					t.Fatal(err)
				}
				findings, err := types.ParseFindingsJSON(boundary.Findings)
				if err != nil || len(findings.Items) != 1 || findings.Items[0].ID != "b" || !boundary.NeedsApproval || boundary.ReviewApprovedHeadSHA != "" || !boundary.ReviewCarryResolved {
					t.Fatalf("B gained clean boundary credit: %+v %v", boundary, err)
				}
				reviews := 0
				for _, invocation := range h.AgentInvocations() {
					if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
						reviews++
					}
				}
				if reviews < 3 || gitIn(t, h, h.UpstreamDir, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch) != "" {
					t.Fatalf("unresolved finding bypassed fresh review/publication guard: reviews=%d", reviews)
				}
				out, err := h.Run("axi", "status", "--run", parked.ID)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("archived resolved boundary: %s\nfresh review retained operator gate:\n%s", *history[0].BoundaryJSON, out)
				h.Respond(parked.ID, types.StepReview, types.ActionAbort)
				if terminal := h.WaitForRun(branch, 30*time.Second); terminal.Status != types.RunFailed {
					t.Fatal("abort did not terminate")
				}
				return
			}
			// This control includes integration plus another validation pass.
			// Leave bounded room for those subprocesses on a loaded host;
			// the exact review-count and publication assertions remain below.
			completed := h.WaitForRun(branch, 3*time.Minute)
			if completed.Status != types.RunCompleted {
				t.Fatalf("clean control did not complete: %s %s", completed.Status, deref(completed.Error))
			}
			reviews := 0
			for _, invocation := range h.AgentInvocations() {
				if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
					reviews++
				}
			}
			published := h.UpstreamBranchSHA(branch)
			if reviews != 2 || published == head || !isAncestorIn(t, h, h.UpstreamDir, target, published) || gitIn(t, h, h.UpstreamDir, "show", published+":review-fix.txt") != "review fix survives" {
				t.Fatalf("clean control lost review credit or fixes: reviews=%d published=%s", reviews, published)
			}
			t.Logf("clean control published %s containing target %s and review fix after exactly two review turns", published, target)
		})
	}
}
