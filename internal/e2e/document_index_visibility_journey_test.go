//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #90 requires refusal even when flagged bytes equal HEAD. Exercise
// that boundary through the real CLI and daemon, with Git as the byte oracle.
func TestDocumentJourneyCleanFlaggedBytesAndEscapedCount(t *testing.T) {
	for _, kind := range []string{"--assume-unchanged", "--skip-worktree", "combined-newline"} {
		t.Run(kind, func(t *testing.T) {
			action := documentJourneyAction("", "")
			count := "1"
			if kind == "combined-newline" {
				const path = "guide\nwith newline.md"
				action["edits"] = []map[string]string{{"path": path, "new": "unchanged guide\n"}}
				action["git"] = [][]string{
					{"add", "--", path}, {"commit", "-m", "add guide"},
					{"update-index", "--assume-unchanged", "--", path},
					{"update-index", "--skip-worktree", "--", path},
					{"update-index", "--assume-unchanged", "--", "feature.txt"},
				}
				count = "2"
			} else {
				action["git"] = [][]string{{"update-index", kind, "--", "feature.txt"}}
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", action)})
			branch, submitted := documentJourneyStart(t, h, "commands:\n  lint: 'true'\n")
			cause := "worktree has " + count + " entries hidden from status by index flags; Document cannot verify a clean entry"
			failed := documentJourneyFailed(t, h, branch, cause)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(failed.RepoID, failed.ID)
			if status := gitIn(t, h, worktree, "status", "--porcelain"); status != "" {
				t.Fatalf("expected apparently clean status, got %q", status)
			}
			flags := gitIn(t, h, worktree, "ls-files", "-v")
			wantFlag := "h feature.txt"
			if kind == "--skip-worktree" {
				wantFlag = "S feature.txt"
			}
			if !strings.Contains(flags, wantFlag) {
				t.Fatalf("refusal cleared flag: %q", flags)
			}
			got, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
			if err != nil || string(got) != "BAD spacing\n" {
				t.Fatalf("refusal changed unchanged bytes: %q, %v", got, err)
			}
			head := gitIn(t, h, worktree, "rev-parse", "HEAD")
			if kind == "combined-newline" {
				if !strings.Contains(*failed.Error, `"guide\nwith newline.md"`) {
					t.Fatalf("refusal did not escape newline path: %s", *failed.Error)
				}
				if commits := gitIn(t, h, worktree, "rev-list", "--count", submitted+"..HEAD"); commits != "1" {
					t.Fatalf("refusal lost guide commit: %s", commits)
				}
			} else if head != submitted {
				t.Fatalf("refusal changed HEAD: %s", head)
			}
			passes := documentJourneyPasses(t, h, failed.ID)
			if len(passes) != 1 || passes[0].ToHeadSHA != nil || passes[0].CommitCount != 0 {
				t.Fatalf("refusal completed pending pass: %+v", passes)
			}
			gateJourneyEvidence(t, "preserved real Git state", map[string]any{
				"flags": flags, "feature_bytes": string(got), "head": head, "error": *failed.Error,
			})
		})
	}
}
