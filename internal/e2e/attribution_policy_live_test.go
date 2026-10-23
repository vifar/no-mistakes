//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is issue #80's commit-time policy and the recorded repair contract:
// actual commits determine the outcome, independently of the agent's prose and
// optional evaluation capture. The forge is disposable; the CLI and daemon are real.
func TestAttributionRepairOutcomeLiveJourney(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, change := range []bool{false, true} {
			t.Run(fmt.Sprintf("capture=%v/change=%v", capture, change), func(t *testing.T) {
				scenario := configurableFixCommitScenario(t, "no changes applied")
				data, err := os.ReadFile(scenario)
				if err != nil {
					t.Fatal(err)
				}
				cleanRereview := `  - match: "This is a re-review after this run's automated fix round(s):"
    text: "review is clean"
    structured:
      findings: []
      summary: "no remaining findings"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
`
				data = []byte(strings.Replace(string(data), "  - match: \"Review the code changes and return structured findings\"", cleanRereview+"  - match: \"Review the code changes and return structured findings\"", 1))
				if change {
					message := "guard unsafe value\n\nBody discusses Generated with Claude Code without being a footer.\n\nCo-Authored-By: gpt-5.5\n <model@example.com>\nCo-Authored-By: Claude Smith <human@example.com>\nCo-Authored-By: Claude <claude@example.com>\nDisclosure: fixtures were generated with the repository recorder.\nClaude-Session: agent\nCodex-Session: agent\nAssisted-by: pi:local\nGenerated with Claude Code"
					gitAction := fmt.Sprintf("    stage: [feature.txt]\n    git:\n      - [commit, --no-verify, -m, %q]\n    structured:\n      summary:", message)
					data = []byte(strings.Replace(string(data), "    structured:\n      summary:", gitAction, 1))
				} else {
					data = []byte(strings.Replace(string(data), "    edits:\n      - path: \"feature.txt\"\n        old: \"unsafe\"\n        new: \"safe\"\n", "", 1))
				}
				if err := os.WriteFile(scenario, data, 0600); err != nil {
					t.Fatal(err)
				}
				h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: fmt.Sprintf("eval:\n  capture_provenance: %v\n  auto_capture: false\n", capture)})
				cfg := filepath.Join(h.NMHome, "config.yaml")
				data, err = os.ReadFile(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				const remote = "https://github.com/example/no-mistakes.git"
				configureGitURLRewrite(t, h, remote, h.UpstreamDir)
				if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
					t.Fatalf("set origin: %v\n%s", err, out)
				}
				ghLog := filepath.Join(filepath.Dir(h.AgentLog), "repair-pr.log")
				t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
				t.Setenv("FAKEAGENT_GH_LOG", ghLog)
				t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
				if out, err := h.Run("init"); err != nil {
					t.Fatalf("init: %v\n%s", err, out)
				}
				const branch = "feature/repair-outcome"
				h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
				h.PushToGate(branch)
				run := h.WaitForRun(branch, 60*time.Second)
				if run.Status != types.RunCompleted {
					t.Fatalf("run = %s, error=%v", run.Status, run.Error)
				}
				if change {
					wantMessage := []byte("guard unsafe value\n\nBody discusses Generated with Claude Code without being a footer.\n\nCo-Authored-By: Claude Smith <human@example.com>\nCo-Authored-By: Claude <claude@example.com>\nDisclosure: fixtures were generated with the repository recorder.\n")
					if got := liveStoredCommitMessage(t, h, h.UpstreamDir, "refs/heads/"+branch); !bytes.Equal(got, wantMessage) {
						t.Fatalf("direct agent commit = %q, want %q", got, wantMessage)
					}
					t.Logf("direct agent commit preserves prose, human coauthors and disclosures while removing folded model attribution and session/footer lines: %q", wantMessage)
				}
				database, err := db.Open(paths.WithRoot(h.NMHome).DB())
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				rounds, err := database.GetRoundsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, round := range rounds {
					if !round.IsFixRound() {
						continue
					}
					found = true
					if round.FixApplied == nil || *round.FixApplied != change || (!capture && round.StartingHeadSHA != nil) {
						t.Fatalf("persisted repair outcome: %+v", round)
					}
					t.Logf("persisted actual repair=%v; evaluation capture=%v; producer summary=%q", *round.FixApplied, capture, *round.FixSummary)
				}
				if !found {
					t.Fatal("no persisted repair round")
				}
				body := createdPRBody(t, readGHStubInvocations(t, ghLog))
				want, forbidden := "🔧 No changes applied.", "auto-fixed"
				if change {
					want, forbidden = "🔧 Fix applied.", "🔧 No changes applied."
					if !strings.Contains(body, "auto-fixed") {
						t.Fatalf("PR heading did not count applied repair:\n%s", body)
					}
				}
				if !strings.Contains(body, want) || strings.Contains(body, forbidden) || strings.Contains(body, "result not reported") {
					t.Fatalf("PR repair outcome contradicts actual changes:\n%s", body)
				}
				t.Logf("real daemon's PR body delivered to disposable forge:\n%s", body)
			})
		}
	}
}

func TestAttributionBypassRefusedLiveJourney(t *testing.T) {
	scenario := configurableFixCommitScenario(t, "guard unsafe value")
	data, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	gitAction := "    stage: [feature.txt]\n    git:\n      - [-c, 'core.hooksPath=', commit, --no-verify, -m, \"bypassed repair\\n\\nCodex-Session: bypass\"]\n    structured:\n      summary:"
	data = []byte(strings.Replace(string(data), "    structured:\n      summary:", gitAction, 1))
	if err := os.WriteFile(scenario, data, 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	cfg := filepath.Join(h.NMHome, "config.yaml")
	data, err = os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/bypass-refusal"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "AI attribution bypassed commit-time filtering") {
		t.Fatalf("bypass run = %s, error=%v", run.Status, run.Error)
	}
	if run.HeadSHA != submitted {
		t.Fatalf("bypassed commit was adopted: %s", run.HeadSHA)
	}
	if out, err := h.runGit(t.Context(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("bypassed branch was published: %s", out)
	}
	t.Logf("real daemon refused bypass before adopting or publishing the correction: %s", *run.Error)
}

func liveStoredCommitMessage(t *testing.T, h *Harness, dir, sha string) []byte {
	t.Helper()
	raw, err := h.runGit(t.Context(), dir, "cat-file", "commit", sha)
	if err != nil {
		t.Fatalf("read commit object: %v\n%s", err, raw)
	}
	_, message, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		t.Fatal("commit message boundary missing")
	}
	return message
}

// A real gate run imports an existing remote feature commit and replays it onto
// main. Original hooks may leave side effects but may not change replay bytes.
// A second real Git replay uses the policy installed by the daemon's real CLI.
func TestAttributionImportedReplayLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, "unused")})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	git := func(dir string, args ...string) []byte {
		t.Helper()
		out, err := h.runGit(t.Context(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	const branch = "feature/imported-replay"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "submitted feature\n\nCodex-Session: submitted")
	git(h.WorkDir, "checkout", "-b", "import-source")
	message := []byte("  imported raw subject  \n\nBody with trailing spaces.  \n# original comment\n\nDisclosure: fixtures were generated with the repository recorder.\nCo-Authored-By: Claude Smith <human@example.com>\nCodex-Session: imported\n\n\n")
	h.CommitChange("import-source", "imported.txt", "existing imported work\n", "temporary message")
	messageFile := filepath.Join(filepath.Dir(h.AgentLog), "original-message")
	if err := os.WriteFile(messageFile, message, 0600); err != nil {
		t.Fatal(err)
	}
	git(h.WorkDir, "commit", "--amend", "--cleanup=verbatim", "-F", messageFile)
	imported := strings.TrimSpace(string(git(h.WorkDir, "rev-parse", "HEAD")))
	git(h.WorkDir, "push", h.UpstreamDir, "HEAD:refs/heads/"+branch)
	h.CommitChange("main", "target.txt", "target advance\n", "advance target")
	git(h.WorkDir, "push", h.UpstreamDir, "main")
	git(h.WorkDir, "checkout", branch)
	hooks := filepath.Join(paths.WithRoot(h.NMHome).RepoDir(h.repoID()), "hooks")
	side := filepath.Join(filepath.Dir(h.AgentLog), "original-hook-effects")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'ran\\n' >> '%s'\nprintf '\\nHook body edit.\\n\\nSigned-off-by: Hook Person <hook@example.com>\\nCodex-Session: hook\\n' >> \"$1\"\n", side)
	if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
	if got := liveStoredCommitMessage(t, h, worktree, "HEAD"); !bytes.Equal(got, message) {
		t.Fatalf("first imported replay bytes = %q, want %q", got, message)
	}
	if parked.HeadSHA != strings.TrimSpace(string(git(worktree, "rev-parse", "HEAD"))) {
		t.Fatal("daemon did not adopt replayed head")
	}
	if effects, err := os.ReadFile(side); err != nil || len(effects) == 0 {
		t.Fatalf("original hook effects = %q, %v", effects, err)
	}
	h.CommitChange("main", "next-target.txt", "second target\n", "advance target again")
	git(h.WorkDir, "push", h.UpstreamDir, "main")
	git(worktree, "fetch", "origin", "main")
	metadata := strings.TrimSpace(string(git(worktree, "rev-parse", "--absolute-git-dir")))
	policyRoot := filepath.Join(metadata, "no-mistakes-attribution")
	parameters := fmt.Sprintf("'core.hooksPath=%s' 'commit.cleanup=verbatim'", policyRoot)
	for _, args := range [][]string{{"rebase", "origin/main"}, {"commit", "--allow-empty", "--no-verify", "-C", imported}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = worktree
		cmd.Env = append(os.Environ(), "GIT_CONFIG_PARAMETERS="+parameters, "GIT_EDITOR=true", "GIT_AUTHOR_NAME=Victor I", "GIT_AUTHOR_EMAIL=vifarinde@gmail.com", "GIT_COMMITTER_NAME=Victor I", "GIT_COMMITTER_EMAIL=vifarinde@gmail.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("live Git %v: %v\n%s", args, err, out)
		}
		got := liveStoredCommitMessage(t, h, worktree, "HEAD")
		if args[0] == "rebase" {
			if !bytes.Equal(got, message) {
				t.Fatalf("successive imported replay bytes = %q, want %q", got, message)
			}
			t.Logf("first and successive imported replays preserve all %d message bytes, including whitespace and final newlines: %q", len(message), got)
		} else if bytes.Contains(got, []byte("Codex-Session:")) || !bytes.Contains(got, []byte("Hook Person")) || !bytes.Contains(got, []byte("Claude Smith")) {
			t.Fatalf("new copied correction was not sanitized while keeping human attribution: %q", got)
		} else {
			t.Logf("new copied correction under the real CLI hook: %q", got)
		}
	}
	// Force a stopped replay, author an independent source-copy correction while
	// stopped, then continue through the submitted pick and a clean imported pick.
	h.CommitChange("main", "feature.txt", "main line\n", "advance conflicting target")
	git(h.WorkDir, "push", h.UpstreamDir, "main")
	git(worktree, "fetch", "origin", "main")
	runHookedGit := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = worktree
		cmd.Env = append(os.Environ(), "GIT_CONFIG_PARAMETERS="+parameters, "GIT_EDITOR=true", "GIT_AUTHOR_NAME=Victor I", "GIT_AUTHOR_EMAIL=vifarinde@gmail.com", "GIT_COMMITTER_NAME=Victor I", "GIT_COMMITTER_EMAIL=vifarinde@gmail.com")
		return cmd.CombinedOutput()
	}
	if out, err := runHookedGit("rebase", "origin/main"); err == nil {
		t.Fatalf("expected a stopped conflict, got success: %s", out)
	}
	if conflicts := strings.TrimSpace(string(git(worktree, "diff", "--name-only", "--diff-filter=U"))); conflicts != "feature.txt" {
		t.Fatalf("stopped replay conflicts = %q", conflicts)
	}
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("main line\nunsafe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(worktree, "add", "feature.txt")
	if out, err := runHookedGit("commit", "--allow-empty", "--only", "-C", imported); err != nil {
		t.Fatalf("new copied correction during stop: %v\n%s", err, out)
	}
	if got := liveStoredCommitMessage(t, h, worktree, "HEAD"); bytes.Contains(got, []byte("Codex-Session:")) {
		t.Fatalf("new correction during stopped replay retained attribution: %q", got)
	}
	if out, err := runHookedGit("rebase", "--continue"); err != nil {
		t.Fatalf("continue stopped replay: %v\n%s", err, out)
	}
	replayedSubmitted := strings.TrimSpace(string(git(worktree, "log", "-1", "--format=%H", "--", "feature.txt")))
	if got, want := liveStoredCommitMessage(t, h, worktree, replayedSubmitted), liveStoredCommitMessage(t, h, worktree, submitted); !bytes.Equal(got, want) {
		t.Fatalf("stopped submitted replay = %q, want %q", got, want)
	}
	replayedImported := strings.TrimSpace(string(git(worktree, "log", "-1", "--format=%H", "--", "imported.txt")))
	if got := liveStoredCommitMessage(t, h, worktree, replayedImported); !bytes.Equal(got, message) {
		t.Fatalf("clean imported pick after conflict = %q, want %q", got, message)
	}
	t.Log("stopped submitted replay and following clean imported pick preserved exact original bytes; new source-copy correction during the stop was sanitized")
	h.CommitChange("main", "merge-target.txt", "merge target\n", "advance merge target")
	git(h.WorkDir, "push", h.UpstreamDir, "main")
	git(worktree, "fetch", "origin", "main")
	if out, err := runHookedGit("merge", "--no-ff", "--no-edit", "origin/main"); err != nil {
		t.Fatalf("automatic merge under the pipeline hook: %v\n%s", err, out)
	}
	if parents := strings.Fields(string(git(worktree, "rev-list", "--parents", "-n", "1", "HEAD"))); len(parents) != 3 {
		t.Fatalf("automatic merge did not retain two parents: %v", parents)
	}
	if got := liveStoredCommitMessage(t, h, worktree, "HEAD"); bytes.Contains(got, []byte("Codex-Session:")) || !bytes.Contains(got, []byte("Signed-off-by: Hook Person")) {
		t.Fatalf("automatic merge attribution policy = %q", got)
	} else {
		t.Logf("new two-parent automatic merge sanitized repository-hook attribution and kept the human signoff: %q", got)
	}
	if got := liveStoredCommitMessage(t, h, worktree, imported); !bytes.Equal(got, message) {
		t.Fatal("original imported object changed")
	}
	t.Logf("submitted commit %s and imported commit %s remain unchanged; original hook side effects survived", submitted, imported)
}
