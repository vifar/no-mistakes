//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The accepted history contract permits a legitimate no-op, but never lets
// persisted findings or commits bypass the required summary on recovery.
// Legacy records and intervening commits are seeded with the owned daemon
// stopped; the resumed admission and agent execution use the real product.
func TestDocumentJourneyRecoveredHistorySummaryContract(t *testing.T) {
	for _, kind := range []string{"no-op then changed input", "findings without summary", "commit without summary"} {
		t.Run(kind, func(t *testing.T) {
			action := documentJourneyAction("", "")
			action["structured"].(map[string]any)["summary"] = ""
			if kind == "commit without summary" {
				action = documentJourneyAction("README.md", "accepted documentation\n")
			}
			scenario := documentJourneyScenario(t, "", action)
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			branch, _ := documentJourneyStart(t, h, "commands:\n  lint: 'false'\n")
			parked := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 90*time.Second)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
			gateJourneyDaemon(t, h, "stop")
			passes := documentJourneyPasses(t, h, parked.ID)
			if len(passes) != 1 || documentJourneyCalls(h) != 1 {
				t.Fatalf("initial pass did not execute: %+v", passes)
			}
			valid := kind == "no-op then changed input"
			if valid {
				if passes[0].CommitCount != 0 {
					t.Fatal("no-op consumed commit budget")
				}
				if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("actual changed inputs\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, h, worktree, "add", "feature.txt")
				gitIn(t, h, worktree, "commit", "-m", "intervening input")
				head := gitIn(t, h, worktree, "rev-parse", "HEAD")
				documentJourneyDB(t, h, func(database *db.DB) {
					if err := database.UpdateRunHeadSHA(parked.ID, head); err != nil {
						t.Fatal(err)
					}
				})
			} else {
				raw := `{"findings":[],"summary":""}`
				from, count := parked.HeadSHA, 0
				if kind == "findings without summary" {
					raw = `{"findings":[{"severity":"warning","description":"persisted gap","action":"ask-user"}],"summary":""}`
				} else {
					if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("legacy repair without summary\n"), 0600); err != nil {
						t.Fatal(err)
					}
					gitIn(t, h, worktree, "add", "README.md")
					gitIn(t, h, worktree, "commit", "-m", "legacy documentation repair")
					parked.HeadSHA, count = gitIn(t, h, worktree, "rev-parse", "HEAD"), 1
				}
				documentJourneyDB(t, h, func(database *db.DB) {
					id, err := database.BeginDocumentPass(parked.ID, from)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.CompleteDocumentPass(id, parked.HeadSHA, count, raw); err != nil {
						t.Fatal(err)
					}
					if err := database.UpdateRunHeadSHA(parked.ID, parked.HeadSHA); err != nil {
						t.Fatal(err)
					}
				})
			}
			documentJourneyScenario(t, scenario, documentJourneyAction("README.md", "new repair after changed inputs\n"))
			documentJourneyReplay(t, h, parked.ID)
			if valid {
				parked = documentJourneyNextGate(t, h, parked.ID, types.StepLint, "")
				passes = documentJourneyPasses(t, h, parked.ID)
				if len(passes) != 2 || passes[1].CommitCount != 1 || documentJourneyCalls(h) != 2 || gitIn(t, h, worktree, "show", "HEAD:README.md") != "new repair after changed inputs" {
					t.Fatalf("valid no-op history blocked a new repair: %+v calls=%d", passes, documentJourneyCalls(h))
				}
				documentJourneyStatus(t, h, parked.ID)
			} else {
				failed := documentJourneyFailed(t, h, branch, "missing summary")
				if len(documentJourneyPasses(t, h, failed.ID)) != 2 || documentJourneyCalls(h) != 1 || gitIn(t, h, worktree, "rev-parse", "HEAD") != parked.HeadSHA {
					t.Fatal("invalid history launched another pass or changed HEAD")
				}
			}
			t.Logf("recovered %s: valid=%t; Document calls=%d", kind, valid, documentJourneyCalls(h))
		})
	}
}
