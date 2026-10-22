//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Git's trailer consumer and exact stored-message bytes are independent
// oracles for the reported no-extra-blank footer layout. Both message hooks
// run through the real CLI installed by the disposable daemon.
func TestAttributionGitFooterLiveJourney(t *testing.T) {
	for _, mode := range []string{"ordinary", "no-verify", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			scenario := configurableFixCommitScenario(t, "repair footer")
			data, err := os.ReadFile(scenario)
			if err != nil {
				t.Fatal(err)
			}
			footer := "Hook body edit.\nSigned-off-by: Hook Person <hook@example.com>\nCodex-Session: hook\nCo-Authored-By: gpt-5.5 <model@example.com>\nOriginated-by: bespoke-bot\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder.\n"
			args := "      - [commit, -m, 'new correction']\n"
			if mode == "no-verify" {
				args = "      - [commit, --no-verify, -m, 'new correction']\n"
			} else if mode == "bypass" {
				args = fmt.Sprintf("      - [-c, 'core.hooksPath=', commit, --no-verify, -m, %q]\n", "new correction\n\n"+footer)
			}
			action := "    stage: [feature.txt]\n    git:\n" + args + "    structured:\n      summary:"
			data = []byte(strings.Replace(string(data), "    structured:\n      summary:", action, 1))
			if err := os.WriteFile(scenario, data, 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "commit:\n  trailers:\n    - 'Originated-by: bespoke-bot'\n"})
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
			oracle := exec.Command("git", "interpret-trailers", "--parse")
			oracle.Dir = h.WorkDir
			oracle.Stdin = strings.NewReader("new correction\n\n" + footer)
			parsed, err := oracle.CombinedOutput()
			if err != nil || !strings.Contains(string(parsed), "Codex-Session: hook\n") || !strings.Contains(string(parsed), "Signed-off-by: Hook Person <hook@example.com>\n") {
				t.Fatalf("Git does not recognize the reported footer: %q, %v", parsed, err)
			}
			t.Logf("Git's independent consumer recognizes the original footer without an extra blank line: %s", parsed)
			const branch = "feature/git-footer"
			submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "submitted feature")
			side := filepath.Join(filepath.Dir(h.AgentLog), "footer-hook-effects")
			if mode != "bypass" {
				hooks := filepath.Join(paths.WithRoot(h.NMHome).RepoDir(h.repoID()), "hooks")
				script := "#!/bin/sh\nprintf 'ran\\n' >> " + shellQuote(side) + "\nprintf '%s' " + shellQuote("\n\n"+footer) + " >> \"$1\"\n"
				for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
					if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			h.PushToGate(branch)
			if mode == "bypass" {
				run := h.WaitForRun(branch, 60*time.Second)
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "AI attribution bypassed commit-time filtering") || run.HeadSHA != submitted {
					t.Fatalf("bypass was not refused before adoption: %+v", run)
				}
				if out, err := h.runGit(t.Context(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
					t.Fatalf("bypass was published: %s", out)
				}
				t.Logf("daemon rejected Git-recognized mixed footer before adoption or publication: %s", *run.Error)
				return
			}
			parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
			cleanFooter := "Hook body edit.\nSigned-off-by: Hook Person <hook@example.com>\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder.\n"
			want := "new correction\n\n\n" + cleanFooter
			calls := 1
			if mode == "ordinary" {
				want += "\n\n" + cleanFooter
				calls = 2
			}
			got := liveStoredCommitMessage(t, h, worktree, "HEAD")
			if string(got) != want {
				t.Fatalf("stored pre-push message = %q, want exact %q", got, want)
			}
			if effects, err := os.ReadFile(side); err != nil || string(effects) != strings.Repeat("ran\n", calls) {
				t.Fatalf("chained hook side effects = %q, %v", effects, err)
			}
			if out, err := h.runGit(t.Context(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
				t.Fatalf("correction was published before inspection: %s", out)
			}
			t.Logf("stored commit before any push; both human attribution and hook body/disclosure preserved, configured and model attribution removed: %q", got)
		})
	}
}
