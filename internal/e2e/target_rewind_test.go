//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A configured Test command advances the disposable bare forge. This makes
// the movement occur strictly after Review and during Test, without sleeps.
func TestTargetAdvanceRewindJourney(t *testing.T) {
	for _, kind := range []string{"fresh branch", "lockfile", "cap", "conflict", "source push"} {
		t.Run(kind, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			root := t.TempDir()
			script := filepath.Join(root, "advance.sh")
			count := filepath.Join(root, "count")
			prep := filepath.Join(root, "prepared")
			extra := fmt.Sprintf("commands:\n  prepare: %q\n  test: %q\nrebase:\n  max_target_advance_rewinds: 3\nauto_fix:\n  rebase: 0\n", "printf 'prepared\\n' >> "+shellQuote(prep), "sh "+shellQuote(script))
			commitTrustedRepoConfig(t, h, extra)
			base := h.UpstreamBranchSHA("main")
			branch := "feature/target-rewind"
			feature := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
			path, content := "target-docs.md", "target documentation\n"
			if kind == "lockfile" {
				path, content = "package-lock.json", "{\"version\":2}\n"
			}
			if kind == "conflict" || kind == "source push" {
				path, content = "feature.txt", "conflicting target content\n"
			}
			advances := 1
			if kind == "cap" {
				advances = 4
			}
			var targetSHAs []string
			for i := 0; i < advances; i++ {
				sha := h.CommitChange("main", path, fmt.Sprintf("%s%d\n", content, i), "advance target during Test")
				gitIn(t, h, h.WorkDir, "push", "origin", sha+fmt.Sprintf(":refs/heads/rewind-fixture-%d", i))
				targetSHAs = append(targetSHAs, sha)
			}
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			h.Checkout(branch)
			var commands strings.Builder
			commands.WriteString("#!/bin/sh\nset -eu\nn=0\nif [ -f " + shellQuote(count) + " ]; then n=$(cat " + shellQuote(count) + "); fi\ncase $n in\n")
			for i, sha := range targetSHAs {
				fmt.Fprintf(&commands, "%d) git --git-dir=%s update-ref refs/heads/main %s ;;\n", i, shellQuote(h.UpstreamDir), sha)
			}
			commands.WriteString("esac\nprintf '%s' \"$((n+1))\" > " + shellQuote(count) + "\n")
			if err := os.WriteFile(script, []byte(commands.String()), 0700); err != nil {
				t.Fatal(err)
			}
			initFromOwnWorktree(t, h, "init-target-rewind")
			h.Checkout(branch)
			h.PushToGate(branch)
			if kind == "conflict" || kind == "source push" {
				parked := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
				if kind == "source push" {
					gitIn(t, h, h.UpstreamDir, "update-ref", "refs/heads/"+branch, targetSHAs[0])
					h.Respond(parked.ID, types.StepRebase, types.ActionFix)
					failed := h.WaitForRun(branch, 60*time.Second)
					if failed.Status != types.RunFailed || failed.Error == nil || !strings.Contains(*failed.Error, "source") {
						t.Fatalf("source push accepted: %s %s", failed.Status, deref(failed.Error))
					}
					return
				}
				for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
					if err := h.RespondError(parked.ID, types.StepRebase, action); err == nil {
						t.Fatalf("pending conflict admitted %s", action)
					}
				}
				if err := os.Rename(h.UpstreamDir, h.UpstreamDir+".offline"); err != nil {
					t.Fatal(err)
				}
				h.Respond(parked.ID, types.StepRebase, types.ActionAbort)
				terminal := h.WaitForRun(branch, 30*time.Second)
				if terminal.Status != types.RunFailed {
					t.Fatalf("abort status=%s", terminal.Status)
				}
				count, pending, history := targetRewindHistory(t, h.NMHome, terminal.ID)
				if count != 1 || history != 1 || pending {
					t.Fatalf("abort did not abandon pending state: count=%d history=%d pending=%t", count, history, pending)
				}
				return
			}
			run := h.WaitForRun(branch, 120*time.Second)
			want := types.RunCompleted
			if kind == "cap" {
				want = types.RunFailed
			}
			if run.Status != want {
				t.Fatalf("journey status=%s error=%s", run.Status, deref(run.Error))
			}

			rewinds, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
			wantCharge := int64(1)
			if kind == "cap" {
				wantCharge = 3
			}
			if rewinds != wantCharge || history != wantCharge {
				t.Fatalf("charge=%d history=%d want %d", rewinds, history, wantCharge)
			}
			if pending {
				t.Fatal("terminal run left a pending rewind")
			}

			if want == types.RunCompleted {
				published := h.UpstreamBranchSHA(branch)
				if published == feature || published != run.HeadSHA {
					t.Fatalf("published head=%s run=%s submitted=%s", published, run.HeadSHA, feature)
				}
				if !isAncestorIn(t, h, h.UpstreamDir, targetSHAs[0], published) {
					t.Fatal("published head omits advanced target")
				}
				preparation, err := os.ReadFile(prep)
				if err != nil || strings.Count(string(preparation), "prepared") != 2 {
					t.Fatalf("preparation did not rerun: %q %v", preparation, err)
				}
				reviews, tests := 0, 0
				for _, inv := range h.AgentInvocations() {
					if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
						reviews++
					}
					if strings.Contains(inv.Prompt, "You are validating a code change") {
						tests++
					}
				}
				if reviews < 2 || tests < 2 {
					t.Fatalf("stale validation reused: reviews=%d tests=%d", reviews, tests)
				}
			}
			t.Logf("run=%s rewinds=%d final=%s head=%s target=%s", run.ID, rewinds, run.Status, run.HeadSHA, h.UpstreamBranchSHA("main"))
		})
	}
}

func TestTargetAdvanceExistingPRJourney(t *testing.T) {
	for _, kind := range []string{"attached without G", "G attached", "unattached"} {
		t.Run(kind, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			state := setupStatefulGitHub(t, h)
			release := filepath.Join(t.TempDir(), "release-test")
			command := "while [ ! -f " + shellQuote(release) + " ]; do sleep 0.05; done"
			commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: %q\nno_ci: true\n", command))
			initFromOwnWorktree(t, h, "init-existing-rewind")
			branch := "feature/existing-rewind"
			head := h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
			gitIn(t, h, h.WorkDir, "push", "origin", branch)
			writeStatefulGH(t, state, statefulGHState{PRs: map[string]*statefulGHPR{branch: {Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "Prior PR", Body: "prior-run body", Base: "main", Head: branch, State: "OPEN"}}})
			if kind == "unattached" {
				head = h.CommitChange(branch, "unpublished.txt", "unpublished\n", "extend unpublished source")
			}
			h.Checkout("main")
			wt := h.AddWorktree(branch)
			type launchResult struct {
				out string
				err error
			}
			launch := make(chan launchResult, 1)
			go func() {
				out, err := h.RunInDir(wt, "axi", "run", "--intent", "validate the existing PR", "--skip", "ci")
				launch <- launchResult{out, err}
			}()
			active := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusRunning, 90*time.Second)

			if kind == "unattached" {
				if active.PRURL != nil {
					t.Fatalf("unpublished head unexpectedly attached to PR: %s", *active.PRURL)
				}
			} else if active.PRURL == nil || *active.PRURL != "https://github.com/example/closes/pull/42" {
				t.Fatal("existing PR was not pinned before Test")
			}
			if kind == "G attached" {
				d, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				err = d.PutCommitStatusPublication(active.ID, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "example/closes", LastState: "pending"})
				_ = d.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			target := advanceMain(t, h, "new-target.md", "docs-only advance\n", "advance target during Test")
			if err := os.WriteFile(release, []byte("ready"), 0600); err != nil {
				t.Fatal(err)
			}
			run := h.WaitForRun(branch, 120*time.Second)
			want := types.RunCompleted
			if kind != "attached without G" {
				want = types.RunFailed
			}
			result := <-launch
			if want == types.RunCompleted && result.err != nil {
				t.Fatalf("CLI run failed: %v\n%s", result.err, result.out)
			}
			if run.Status != want {
				t.Fatalf("existing-PR journey status=%s error=%s", run.Status, deref(run.Error))
			}

			rewinds, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
			wantRewinds := int64(0)
			if want == types.RunCompleted {
				wantRewinds = 1
			}
			if rewinds != wantRewinds || history != wantRewinds || pending {
				t.Fatalf("existing PR history: charge=%d rows=%d pending=%t", rewinds, history, pending)
			}
			if want == types.RunCompleted {
				if !isAncestorIn(t, h, h.UpstreamDir, target, h.UpstreamBranchSHA(branch)) {
					t.Fatal("existing PR publication omitted the advanced target")
				}
				body := livePRBody(t, state, branch)
				if body == "prior-run body" || !strings.Contains(body, `"head_sha":"`+run.HeadSHA+`"`) || run.PRURL == nil || *run.PRURL != "https://github.com/example/closes/pull/42" {
					t.Fatalf("PR step did not publish current run body: %s", body)
				}
			} else if run.Error == nil || !strings.Contains(*run.Error, "start a new run") {
				t.Fatalf("wrong refusal: %s", deref(run.Error))
			}
		})
	}
}

// Read raw durable history rather than relying on the pipeline's own getters.
func targetRewindHistory(t *testing.T, nmHome, runID string) (rewinds int64, pending bool, history int64) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+filepath.Join(nmHome, "state.sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = database.QueryRow(`SELECT target_advance_rewinds,pending_target_rewind_id IS NOT NULL FROM runs WHERE id=?`, runID).Scan(&rewinds, &pending); err != nil {
		t.Fatal(err)
	}
	if err = database.QueryRow(`SELECT count(*) FROM run_target_rewinds WHERE run_id=?`, runID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	return
}

// A CI support claim makes a published repair return to Review. The next Test
// advances the target: publication provenance must keep this run outside L.
func TestTargetAdvancePublishedCIRestartJourney(t *testing.T) {
	scenario := cleanReviewScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "ci_timeout: 120s\n"})
	setupStatefulGitHub(t, h)
	script := filepath.Join(t.TempDir(), "test-after-ci.sh")
	count := script + ".count"
	commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: %q\nci:\n  rerun_transient: 0\n  revalidate_repairs: false\nauto_fix:\n  ci: 1\n", "sh "+shellQuote(script)))
	initFromOwnWorktree(t, h, "init-published-rewind")
	base := h.UpstreamBranchSHA("main")
	branch := "feature/published-ci-restart"
	head := h.CommitChange(branch, "feature.txt", "feature\n", "submit feature")
	target := h.CommitChange("main", "later-target.md", "later target\n", "prepare post-CI target")
	gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/target-after-ci")
	gitIn(t, h, h.WorkDir, "reset", "--hard", base)
	h.Checkout(branch)
	command := "#!/bin/sh\nset -eu\nn=0\nif [ -f " + shellQuote(count) + " ]; then n=$(cat " + shellQuote(count) + "); fi\nif [ \"$n\" -gt 0 ]; then git --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/main " + target + "; fi\nprintf '%s' \"$((n+1))\" > " + shellQuote(count) + "\n"
	if err := os.WriteFile(script, []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	custom := `actions:
  - match: "The following CI checks have failed"
    edits:
      - path: ci-fixed.txt
        new: "CI repair\n"
    structured:
      summary: "repair the fixture"
      code_change_needed: true
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: ci-claim
          severity: error
          action: auto-fix
          description: "fixture CI check reports failure"
          review_scope: source
          support:
            claim_type: ci
            ci:
              check_id: "1"
              head_sha: "` + head + `"
      summary: "CI owner must verify this claim"
      risk_level: high
      risk_rationale: "CI claim needs provider proof"
      risk_scope: source-or-external
`
	if err := os.WriteFile(scenario, []byte(strings.Replace(string(old), "actions:\n", custom, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	delegateDir := filepath.Join(h.BinDir, "ci-delegate")
	if err := os.MkdirAll(delegateDir, 0700); err != nil {
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
	checks := `{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"fixture-build","status":"COMPLETED","conclusion":"FAILURE","checkSuite":{"app":{"slug":"fixture-ci"}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}}`
	wrapper := "#!/bin/sh\ncase \"$*\" in\n*'statusCheckRollup'*) printf '%s\\n' " + shellQuote(checks) + "; exit 0 ;;\n*'mergeable,mergeStateStatus'*) printf 'MERGEABLE CLEAN\\n'; exit 0 ;;\nesac\nexec " + shellQuote(delegate) + " \"$@\"\n"
	if err := os.WriteFile(gh, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 150*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "PR comparison changed after rebase") {
		t.Fatalf("published restart did not retain refusal: %s %s", run.Status, deref(run.Error))
	}
	d, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PushGeneration == nil || *stored.PushGeneration < 2 {
		t.Fatalf("CI repair was not published before restart: %+v", stored.PushGeneration)
	}
	if h.UpstreamBranchSHA("main") != target {
		t.Fatal("Test after CI restart never advanced the target")
	}
	reviews := 0
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
			reviews++
		}
	}
	if reviews < 2 {
		t.Fatalf("CI did not restart Review: %d review invocations", reviews)
	}
	rewinds, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
	if rewinds != 0 || pending || history != 0 {
		t.Fatalf("published restart was rewound: %d %t %d", rewinds, pending, history)
	}
}
