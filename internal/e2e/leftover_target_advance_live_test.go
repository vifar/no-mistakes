//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the author's preservation contract: one attributed correction,
// reviewed before publication, with scratch preserved on disk and absent from
// Git. A second real edit must be refused without destroying either file.
func TestLeftoverTargetAdvanceLiveJourney(t *testing.T) {
	for _, kind := range []string{"post Test", "spent budget scratch", "second real edit", "custom gate", "versioned upgrade", "legacy upgrade"} {
		t.Run(kind, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			root := filepath.Dir(h.AgentLog)
			script := filepath.Join(root, "edits.sh")
			count := filepath.Join(root, "count")
			command := "sh " + shellQuote(script)
			config := fmt.Sprintf("commands:\n  test: %q\n  lint: 'true'\n", command)
			if kind == "custom gate" {
				config = fmt.Sprintf("commands:\n  test: 'true'\n  lint: 'true'\ngates:\n  - name: validation\n    after: test\n    command: %q\n", command)
			}
			commitTrustedRepoConfig(t, h, config)
			base := h.UpstreamBranchSHA("main")
			target := h.CommitChange("main", "landed.txt", "landed target\n", "prepare advance")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/leftover-advance"
			head := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
			advanceAt := 1
			if kind == "spent budget scratch" || kind == "second real edit" {
				advanceAt = 2
			}
			commands := fmt.Sprintf("#!/bin/sh\nset -eu\nn=0\nif [ -f %s ]; then n=$(cat %s); fi\nn=$((n+1))\nprintf '%%s' \"$n\" > %s\nif [ \"$n\" = 1 ]; then printf 'preserved validation edit\\n' > feature.txt; fi\n", shellQuote(count), shellQuote(count), shellQuote(count))
			if kind == "spent budget scratch" || kind == "second real edit" {
				commands += "mkdir -p .journal-live\nprintf 'scratch retained\\n' > .journal-live/postgres.log\ngit add .journal-live/postgres.log\n"
			}
			if kind == "second real edit" {
				commands += "if [ \"$n\" = 2 ]; then printf 'second edit retained\\n' > feature.txt; fi\n"
			}
			commands += fmt.Sprintf("if [ \"$n\" = %d ]; then git --git-dir=%s update-ref refs/heads/main %s; fi\n", advanceAt, shellQuote(h.UpstreamDir), target)
			if err := os.WriteFile(script, []byte(commands), 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "versioned upgrade" || kind == "legacy upgrade" {
				path := filepath.Join(h.NMHome, "state.sqlite")
				seed, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := seed.Close(); err != nil {
					t.Fatal(err)
				}
				legacy, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				version := 4
				if kind == "legacy upgrade" {
					version = 0
				}
				_, err = legacy.Exec(fmt.Sprintf("ALTER TABLE runs DROP COLUMN leftover_validation_commits; PRAGMA user_version=%d", version))
				closeErr := legacy.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("prepare historical database: %v %v", err, closeErr)
				}
			}
			initFromOwnWorktree(t, h, "init-leftover-advance")
			h.Checkout(branch)
			h.PushToGate(branch)
			if kind == "second real edit" {
				run := h.WaitForRun(branch, 90*time.Second)
				worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
				for path, want := range map[string]string{"feature.txt": "second edit retained\n", ".journal-live/postgres.log": "scratch retained\n"} {
					got, err := os.ReadFile(filepath.Join(worktree, path))
					if err != nil || string(got) != want {
						t.Fatalf("refusal lost %s: %q %v", path, got, err)
					}
				}
				out, err := h.Run("axi", "status", "--run", run.ID)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("bounded refusal retained second edit and scratch:\n%s", out)
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "test step left uncommitted edits") || !strings.Contains(*run.Error, "feature.txt") || strings.Contains(*run.Error, ".journal-live") {
					t.Fatalf("second real edit was not refused with attribution: %s %s", run.Status, deref(run.Error))
				}
				assertLeftoverSnapshotSurvivesCleanup(t, h, run, "+second edit retained")
				return
			}
			run := h.WaitForRun(branch, 90*time.Second)
			if run.Status != types.RunCompleted {
				out, _ := h.Run("axi", "status", "--run", run.ID)
				t.Logf("terminal product status:\n%s", out)
				worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
				t.Logf("retained worktree status:\n%s", gitIn(t, h, worktree, "status", "--porcelain"))
				if scratch, err := os.ReadFile(filepath.Join(worktree, ".journal-live/postgres.log")); err == nil {
					t.Logf("retained scratch content: %q", scratch)
				}
				if published, err := h.runGit(t.Context(), h.UpstreamDir, "show", "refs/heads/"+branch+":feature.txt"); err == nil {
					t.Logf("published feature.txt content: %q", published)
				}
				if log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "rebase.log")); err == nil {
					t.Logf("Rebase log:\n%s", log)
				}
				t.Fatalf("preservation failed: %s %s", run.Status, deref(run.Error))
			}
			if kind == "spent budget scratch" {
				worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
				if got, err := os.ReadFile(filepath.Join(worktree, ".journal-live/postgres.log")); err != nil || string(got) != "scratch retained\n" {
					t.Fatalf("completion lost retained scratch: %q %v", got, err)
				}
			}
			if kind == "post Test" {
				assertLeftoverSnapshotSurvivesCleanup(t, h, run, "+preserved validation edit")
			}
			published := h.UpstreamBranchSHA(branch)
			if published != run.HeadSHA || !isAncestorIn(t, h, h.UpstreamDir, target, published) {
				t.Fatal("published branch lost the advanced target")
			}
			if got := gitIn(t, h, h.UpstreamDir, "show", published+":feature.txt"); got != "preserved validation edit" {
				t.Fatalf("validation edit was lost: %q", got)
			}
			if got := gitIn(t, h, h.UpstreamDir, "ls-tree", "-r", "--name-only", published, "--", ".journal-live"); got != "" {
				t.Fatalf("scratch entered published tree: %s", got)
			}
			subjects := gitIn(t, h, h.UpstreamDir, "log", "--format=%s", base+".."+published)
			producer := "test"
			if strings.Count(subjects, "keep "+producer+" validation edits") != 1 {
				t.Fatalf("correction not uniquely attributed to %s: %s", producer, subjects)
			}
			if published == head {
				t.Fatal("correction did not change the head")
			}
			logTargetRewindStatus(t, h, run.ID)
			t.Logf("published %s contains target %s, feature.txt=preserved validation edit, no .journal-live paths, and one %s correction:\n%s", published, target, producer, subjects)
			for _, step := range []string{"review", "rebase", "document"} {
				if data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, step+".log")); err == nil {
					t.Logf("product %s log:\n%s", step, data)
				}
			}
		})
	}
}

func TestDocumentInheritedScratchRollbackLiveJourney(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial_staged_%t", staged), func(t *testing.T) {
			action := documentJourneyAction("guide.md", "rejected proposal\n")
			action["git"] = [][]string{{"add", "-A"}, {"commit", "-m", "first proposal"}, {"commit", "--allow-empty", "-m", "second proposal"}}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", action)})
			command := "mkdir -p .journal-live; printf 'inherited scratch\\n' > .journal-live/postgres.log"
			if staged {
				command += "; git add .journal-live/postgres.log"
			}
			branch, head := documentJourneyStart(t, h, fmt.Sprintf("commands:\n  test: %q\n  lint: 'true'\ndocument:\n  max_commits_per_run: 1\n", command))
			run := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
			if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != head {
				t.Fatalf("rejected commits survived: %s", got)
			}
			if _, err := os.Stat(filepath.Join(worktree, "guide.md")); !os.IsNotExist(err) {
				t.Fatalf("proposal file survived rollback: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(worktree, ".journal-live/postgres.log")); err != nil || string(got) != "inherited scratch\n" {
				t.Fatalf("rollback destroyed inherited scratch: %q %v", got, err)
			}
			if committed := gitIn(t, h, worktree, "ls-tree", "-r", "--name-only", "HEAD", "--", ".journal-live"); committed != "" {
				t.Fatalf("rollback kept scratch in the committed tree: %s", committed)
			}
			out, err := h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("real Document rejected both proposal commits, restored head %s, removed guide.md, retained inherited scratch bytes outside the committed tree:\n%s", head, out)
		})
	}
}

// Issue #113 lost its worktree before anyone could list the edits. Exercise
// the real executor's private record on both success and bounded refusal.
func assertLeftoverSnapshotSurvivesCleanup(t *testing.T, h *Harness, run *db.Run, wantPatch string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(h.NMHome, "logs", run.ID, "leftover-validation-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("missing durable leftover evidence: %v %v", files, err)
	}
	var snapshot struct {
		Producer      string `json:"producer"`
		Status        []byte `json:"status"`
		UnstagedPatch []byte `json:"unstaged_patch"`
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Producer != "test" || !strings.Contains(string(snapshot.Status), "feature.txt") || !strings.Contains(string(snapshot.UnstagedPatch), wantPatch) {
		t.Fatalf("snapshot lost producer, paths or edit bytes: %+v", snapshot)
	}
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(files[0]); err != nil || string(got) != string(data) {
		t.Fatalf("worktree cleanup lost the snapshot: %v", err)
	}
}
