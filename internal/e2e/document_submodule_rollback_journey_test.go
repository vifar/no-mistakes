//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The owner-required rollback contract is the oracle: recursive Git settings
// must never erase nested staged or unstaged work. Exercise both discard paths
// in the real daemon, then join terminal and startup cleanup before inspecting
// the preserved state. Agent edits are simulated; Git and rollback are real.
func TestDocumentJourneyRecursiveSubmoduleRollback(t *testing.T) {
	for _, reason := range []string{"over-budget", "repeated"} {
		for _, state := range []string{"dirty", "clean"} {
			t.Run(reason+"/"+state, func(t *testing.T) {
				scenario := documentJourneyScenario(t, "", nil)
				h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
				source := t.TempDir()
				gitIn(t, h, source, "init", "-b", "main")
				gitIn(t, h, source, "config", "user.name", "E2E")
				gitIn(t, h, source, "config", "user.email", "e2e@example.invalid")
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("original nested guide\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, h, source, "add", ".")
				gitIn(t, h, source, "commit", "-m", "initial module")
				childBase := gitIn(t, h, source, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(source, "version.txt"), []byte("next version\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, h, source, "add", ".")
				gitIn(t, h, source, "commit", "-m", "next module version")
				gitIn(t, h, h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "add", source, "dependency")
				gitIn(t, h, h.WorkDir, "commit", "-am", "initialize dependency")
				limit := "1"
				if reason == "repeated" {
					limit = "3"
					action := documentJourneyAction("README.md", "accepted guide\n", "retained documentation gap")
					action["git"] = [][]string{{"-C", "dependency", "checkout", "--detach", childBase}, {"add", "README.md", "dependency"}, {"commit", "-m", "accept guide and dependency update"}}
					documentJourneyScenario(t, scenario, action)
				}
				branch, submitted := documentJourneyStart(t, h, documentJourneyBoundary+"commands:\n  lint: 'true'\ndocument:\n  max_commits_per_run: "+limit+"\n")
				boundary := types.CustomGateStepName(types.StepTest, "boundary")
				parked := waitForStepStatus(t, h, branch, boundary, types.StepStatusAwaitingApproval, 90*time.Second)
				worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
				gitIn(t, h, worktree, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "dependency")
				gitIn(t, h, worktree, "config", "submodule.recurse", "true")
				accepted := submitted
				if reason == "repeated" {
					h.Respond(parked.ID, boundary, types.ActionApprove)
					parked = waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
					accepted = parked.HeadSHA
					if accepted == submitted {
						t.Fatal("first repair did not commit")
					}
				}
				child := filepath.Join(worktree, "dependency")
				childHead := gitIn(t, h, child, "rev-parse", "HEAD")
				action := documentJourneyAction("README.md", "discarded guide\n")
				edits := []map[string]string{{"path": "README.md", "new": "discarded guide\n"}}
				commands := [][]string{}
				if state == "dirty" {
					edits = append(edits, map[string]string{"path": "dependency/README.md", "new": "unstaged nested bytes\n"}, map[string]string{"path": "dependency/staged.md", "new": "staged nested bytes\n"})
					commands = append(commands, []string{"-C", "dependency", "add", "staged.md"})
				}
				commands = append(commands, []string{"add", "README.md"}, []string{"commit", "-m", "candidate guide"})
				if reason == "over-budget" {
					edits = append(edits, map[string]string{"path": "guide-b.md", "new": "second candidate\n"})
					commands = append(commands, []string{"add", "guide-b.md"}, []string{"commit", "-m", "second candidate"})
				}
				action["edits"], action["git"] = edits, commands
				documentJourneyScenario(t, scenario, action)
				if reason == "repeated" {
					documentJourneyFix(t, h, parked)
				} else {
					h.Respond(parked.ID, boundary, types.ActionApprove)
				}
				if state == "dirty" {
					parked = documentJourneyFailed(t, h, branch, "remain in the worktree")
					got, err := os.ReadFile(filepath.Join(child, "README.md"))
					if err != nil || string(got) != "unstaged nested bytes\n" {
						t.Fatalf("nested worktree bytes lost: %q %v", got, err)
					}
					if got := gitIn(t, h, child, "show", ":staged.md"); got != "staged nested bytes" {
						t.Fatalf("nested staged bytes lost: %q", got)
					}
					if status := gitIn(t, h, child, "status", "--porcelain"); status != "M README.md\nA  staged.md" {
						t.Fatalf("nested index/worktree state changed: %q", status)
					}
				} else {
					previous := ""
					if reason == "repeated" {
						step, _ := findStep(parked.Steps, types.StepDocument)
						previous = step.Gate.Nonce
					}
					parked = documentJourneyNextGate(t, h, parked.ID, types.StepDocument, previous)
					if got := gitIn(t, h, worktree, "status", "--porcelain"); got != "" {
						t.Fatalf("clean rollback left dirt: %s", got)
					}
				}
				if got := gitIn(t, h, child, "rev-parse", "HEAD"); got != childHead {
					t.Fatalf("rollback moved nested HEAD: %s want %s", got, childHead)
				}
				if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != accepted {
					t.Fatalf("rollback did not restore accepted head: %s want %s", got, accepted)
				}
				passes := documentJourneyPasses(t, h, parked.ID)
				last := passes[len(passes)-1]
				if last.CommitCount != 0 || (last.ToHeadSHA == nil) != (state == "dirty") {
					t.Fatalf("rollback recorded wrong completion/accounting: %+v", passes)
				}
				t.Logf("%s rollback/%s: accepted parent HEAD restored; nested HEAD retained; pending=%t; staged and unstaged work preserved after cleanup", reason, state, last.ToHeadSHA == nil)
			})
		}
	}
}
