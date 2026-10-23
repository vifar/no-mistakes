//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// The oracle is issue #98 and the recorded publication ruling: Git keeps the
// human trailer's bytes, while the published narrative AND quoted evidence use
// the labelled field. A shared loss of the human or leak of the AI fails here.
// Only the external agent and forge are simulated; CLI, daemon, Git and storage
// run normally in the harness's disposable repositories.
func TestReviewFixPublicationLiveJourney(t *testing.T) {
	for _, name := range []string{"confirmed-repair", "legacy-unreported-repair", "fallback-agent-repair"} {
		legacy := name == "legacy-unreported-repair"
		t.Run(name, func(t *testing.T) {
			for key, value := range map[string]string{
				"GIT_AUTHOR_NAME": "Victor I", "GIT_AUTHOR_EMAIL": "vifarinde@gmail.com",
				"GIT_COMMITTER_NAME": "Victor I", "GIT_COMMITTER_EMAIL": "vifarinde@gmail.com",
			} {
				t.Setenv(key, value)
			}
			const disclosure = "Disclosure: fixtures were generated with the repository recorder."
			const human = "Co-Authored-By: Ada & Bob <ada@example.com>"
			const signoff = "Signed-off-by: Victor I <vifarinde@gmail.com>"
			const prose = "Ordinary prose mentions the Co-Authored-By: field without attributing anyone."
			const link = "[Co-Authored-By: field](https://example.com)."
			const wrapped = "**Co-Authored-By: Codex <wrapped-ai@example.com>**.\n\n`co-author (quoted): Codex\n <folded-ai@example.com>`.!\n\n**Co-Authored-By: Jane <jane@example.com>**.\n\n`co-author (quoted): Jane\n <jane@example.com>`.!"
			const payload = `{"ai":"**Co-Authored-By: Codex <payload-ai@example.com>**.","human":"**Co-Authored-By: Jane <jane@example.com>**.","escape":"A\u0042\/C"}`
			// Quoted recorded payloads use the evidence neutralization notice;
			// the identity is removed while unrelated serialized bytes survive.
			const wantPayload = `{"ai":"<no-mistakes: AI attribution line neutralized>","human":"**co-author (quoted): Jane <jane@example.com>**.","escape":"A\u0042\/C"}`
			const footer = "🤖 Generated with [Claude Code](https://claude.ai/code)"
			const rationale = "Red first: unsafe value was accepted.\nGreen: focused validation passed."
			summary := "Guard unsafe value\n\n" + rationale + "\n\n" + disclosure + "\n\n" + human + "\n" + signoff + "\nCo-Authored-By: Codex <commit-ai@example.com>\nClaude-Session: fixture\n" + footer
			const tail = "{\n \"large\": \"cutoff }\n... [20000 bytes truncated] ...\ncontinued text\", \"ai\": \"<code>co-author (quoted): <code>Codex</code> <tail-ai@example.com></code>\", \"human\": \"Co-Authored-By: Ada & Bob <ada@example.com>\", \"escape\": \"A\\u0042\\/C\"\n}"
			const wantTail = "{\n \"large\": \"cutoff }\n... [20000 bytes truncated] ...\ncontinued text\", \"ai\": \"<no-mistakes: AI attribution line neutralized>\", \"human\": \"co-author (quoted): Ada & Bob <ada@example.com>\", \"escape\": \"A\\u0042\\/C\"\n}"
			raw := prose + "\n" + disclosure + "\n" + human + "\n> Co-Authored-By: Claude\n- Co-Authored-By: Codex\n`Co-Authored-By: ChatGPT`\n**Co-Authored-By: Gemini**\n> Co-Authored-By: Jane Doe\n\n[\n \"Co-Authored-By: Codex <array-ai@example.com>\",\n \"Co-Authored-By: Ada & Bob <ada@example.com>\"\n]\n\n```json\n" + tail + "\n```\n\n" + footer
			raw += "\n\n" + link + "\n\n" + wrapped + "\n\n```json\n" + payload + "\n```"
			// The public key must be labelled even when only the key carries
			// markup. Preserve human bytes; equivalent AI entries disappear.
			var labelledKeys []string
			for _, key := range []string{"**Co-Authored-By**:", "`Co-Authored-By`:", "<code>Co-Authored-By</code>:", "&lt;code&gt;Co-Authored-By&lt;/code&gt;:"} {
				for _, suffix := range []string{"", " <jane@example.com>.", "\n <jane@example.com>!"} {
					raw += "\n\n" + key + " Jane" + suffix + "\n\n" + key + " Codex" + strings.ReplaceAll(suffix, "jane@example.com", "key-ai@example.com")
					labelledKeys = append(labelledKeys, strings.Replace(key, "Co-Authored-By", "co-author (quoted)", 1)+" Jane"+suffix)
				}
			}
			clean := map[string]any{
				"findings": []any{}, "summary": "no issues found", "risk_level": "low",
				"risk_rationale": "disposable fixture", "risk_scope": "source-or-external",
			}
			actions := []any{
				map[string]any{
					"match":      "Investigate previous review findings",
					"edits":      []any{map[string]any{"path": "feature.txt", "old": "unsafe", "new": "safe"}},
					"structured": map[string]any{"summary": summary, "verification": "passed"},
				},
				map[string]any{
					"match": "Review the code changes and return structured findings",
					"structured": map[string]any{
						"findings": []any{map[string]any{"id": "guard-1", "file": "feature.txt", "line": 1, "severity": "warning", "action": "auto-fix", "description": "unsafe value needs validation"}},
						"summary":  "found one issue", "risk_level": "medium", "risk_rationale": "fixture guard", "risk_scope": "source-or-external",
					},
				},
				map[string]any{
					"match": "You are validating a code change by driving the product itself.",
					"structured": map[string]any{
						"findings": []any{}, "summary": "disposable rendering input", "testing_summary": "External response fixture for publication validation.",
						"tested": []string{"publication fixture"}, "verdict": "go",
						"scenarios": []any{map[string]any{"name": "external response fixture", "result": "untested", "live": false, "evidence": "disposable fixture", "reason": "Agent response is fixture data; the enclosing journey tests real publication."}},
						"artifacts": []any{map[string]any{"label": "Quoted publication fixture", "kind": "log", "content": raw}},
					},
				},
				map[string]any{
					"match":      "Draft a pull request title and summary for the full branch delta.",
					"structured": map[string]any{"title": "fix: preserve repair rationale\n\n" + footer, "body": "## What Changed\n\nPreserve the repair rationale.\n\n" + raw},
				},
				map[string]any{"structured": clean},
			}
			if !legacy {
				actions = append(actions[:1], append([]any{map[string]any{
					"match": "This is a re-review after this run's automated fix round(s):", "structured": clean,
				}}, actions[1:]...)...)
			}
			data, err := yaml.Marshal(map[string]any{"actions": actions})
			if err != nil {
				t.Fatal(err)
			}
			scenario := filepath.Join(t.TempDir(), "publication.yaml")
			if err := os.WriteFile(scenario, data, 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			cfg := filepath.Join(h.NMHome, "config.yaml")
			data, err = os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			configText := strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)
			if name == "fallback-agent-repair" {
				unavailable := filepath.Join(h.BinDir, "unavailable-codex")
				if err := os.WriteFile(unavailable, []byte("#!"+filepath.Join(h.BinDir, "absent-interpreter")+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				configText = strings.Replace(configText, "agent: claude\n", "agent: [codex, claude]\n", 1)
				configText = strings.Replace(configText, "agent_path_override:\n", "agent_path_override:\n  codex: "+unavailable+"\n", 1)
			}
			if err := os.WriteFile(cfg, []byte(configText), 0600); err != nil {
				t.Fatal(err)
			}
			const remote = "https://github.com/example/no-mistakes.git"
			configureGitURLRewrite(t, h, remote, h.UpstreamDir)
			if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
				t.Fatalf("set local-rewritten origin: %v\n%s", err, out)
			}
			ghLog := filepath.Join(filepath.Dir(h.AgentLog), "publication.log")
			t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
			t.Setenv("FAKEAGENT_GH_LOG", ghLog)
			t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("isolated init: %v\n%s", err, out)
			} else {
				t.Logf("no-mistakes init:\n%s", out)
			}
			const branch = "feature/review-publication"
			h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
			if out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch); err != nil {
				t.Fatalf("isolated gate push: %v\n%s", err, out)
			} else {
				t.Logf("git push no-mistakes %s:\n%s", branch, out)
			}
			if legacy {
				parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
				// Seed the supported old storage shape, which has no applied flag.
				// The live PR renderer must not infer success from summary prose.
				control := openControlDB(t, filepath.Join(h.NMHome, "state.sqlite"))
				defer control.Close()
				result, err := control.Exec("UPDATE step_rounds SET fix_applied = NULL WHERE step_result_id IN (SELECT id FROM step_results WHERE run_id = ?) AND fix_summary IS NOT NULL", parked.ID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("seed one legacy round: rows=%d, err=%v", n, err)
				}
				if out, err := h.Run("axi", "respond", "--action", "approve"); err != nil {
					t.Fatalf("isolated Review approval: %v\n%s", err, out)
				} else {
					t.Logf("no-mistakes axi respond --action approve:\n%s", out)
				}
			}
			run := h.WaitForRun(branch, 60*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run = %s, error=%v", run.Status, run.Error)
			}
			if name == "fallback-agent-repair" {
				control := openControlDB(t, filepath.Join(h.NMHome, "state.sqlite"))
				var failures, successes int
				if err := control.QueryRow("SELECT COUNT(*) FROM agent_invocations WHERE run_id = ? AND step_name = 'pr' AND agent = 'codex' AND exit_status != 'ok'", run.ID).Scan(&failures); err != nil {
					t.Fatal(err)
				}
				if err := control.QueryRow("SELECT COUNT(*) FROM agent_invocations WHERE run_id = ? AND step_name = 'pr' AND agent = 'claude' AND exit_status = 'ok'", run.ID).Scan(&successes); err != nil {
					t.Fatal(err)
				}
				if failures == 0 || successes == 0 {
					t.Fatalf("PR fallback was not exercised: codex failures=%d, claude successes=%d", failures, successes)
				}
				t.Logf("Persisted PR agent attempts: codex startup failures=%d; claude successes=%d", failures, successes)
			}
			message := liveStoredCommitMessage(t, h, h.UpstreamDir, "refs/heads/"+branch)
			wantMessage := "no-mistakes(review): Guard unsafe value\n\nGuard unsafe value\n\nRed first: unsafe value was accepted.\nGreen: focused validation passed.\n\nDisclosure: fixtures were generated with the repository recorder.\n\nRound findings:\n- feature.txt: unsafe value needs validation\n\nCo-Authored-By: Ada & Bob <ada@example.com>\n" + signoff + "\n"
			if !bytes.Equal(message, []byte(wantMessage)) {
				t.Fatalf("published commit bytes = %q, want %q", message, wantMessage)
			}
			trailers, err := h.runGit(t.Context(), h.UpstreamDir, "show", "-s", "--format=%(trailers:only,unfold)", "refs/heads/"+branch)
			if err != nil || strings.TrimSpace(string(trailers)) != human+"\n"+signoff {
				t.Fatalf("Git trailer parser = %q, %v", trailers, err)
			}
			invocations := readGHStubInvocations(t, ghLog)
			body := createdPRBody(t, invocations)
			for _, want := range labelledKeys {
				if !strings.Contains(body, want) {
					t.Errorf("PR lost labelled human key %q", want)
				}
			}
			for _, want := range []string{prose, link, disclosure, "co-author (quoted): Ada & Bob <ada@example.com>", "co-author (quoted): Jane Doe", "**co-author (quoted): Jane <jane@example.com>**.", "`co-author (quoted): Jane\n <jane@example.com>`.!", wantPayload, wantTail, "Preserve the repair rationale."} {
				if !strings.Contains(body, want) {
					t.Errorf("PR lost required published bytes %q", want)
				}
			}
			// Evidence captions use the existing indentation/fence sanitizer.
			// Check the public evidence block itself, independently of narrative.
			_, evidence, found := strings.Cut(body, "<summary>Evidence: Quoted publication fixture</summary>")
			if !found {
				t.Fatal("quoted evidence missing from published PR")
			}
			for _, want := range []string{prose, disclosure, `"human": "co-author (quoted): Ada & Bob <ada@example.com>"`, `"escape": "A\u0042\/C"`, `"ai": "<no-mistakes: AI attribution line neutralized>"`} {
				if !strings.Contains(evidence, want) {
					t.Errorf("quoted evidence lost required bytes %q", want)
				}
			}
			for _, forbidden := range []string{"Codex", "ChatGPT", "Gemini", "Claude", "commit-ai@example.com", "tail-ai@example.com", "array-ai@example.com", "wrapped-ai@example.com", "folded-ai@example.com", "payload-ai@example.com", "key-ai@example.com", "claude.ai/code", "Claude-Session:", `\u003c`, `\u0026`} {
				if strings.Contains(body, forbidden) {
					t.Errorf("PR leaked attribution or escaped human bytes %q", forbidden)
				}
			}
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "Co-Authored-By:") {
					t.Errorf("PR contains a raw trailer: %q", line)
				}
			}
			wantOutcome := "1 issue found → auto-fixed"
			if legacy {
				wantOutcome = "🔧 Fix attempted; result not reported."
				if strings.Contains(body, "auto-fixed") {
					t.Error("legacy prose alone was classified as applied")
				}
			}
			if !strings.Contains(body, wantOutcome) {
				t.Errorf("PR lost expected outcome %q", wantOutcome)
			}
			foundTitle := false
			for _, inv := range invocations {
				if len(inv.Args) < 2 || inv.Args[0] != "pr" || inv.Args[1] != "create" {
					continue
				}
				for i, arg := range inv.Args {
					if arg == "--title" && i+1 < len(inv.Args) {
						foundTitle = true
						if inv.Args[i+1] != "fix: preserve repair rationale" {
							t.Errorf("published PR title = %q", inv.Args[i+1])
						}
					}
				}
			}
			if !foundTitle {
				t.Error("forge did not receive a title")
			}
			t.Logf("CLI journey completed with disposable origin; legacy round seeded=%v", legacy)
			t.Logf("git show -s --format=%%B refs/heads/%s (stored bytes):\n%s", branch, message)
			t.Logf("git show -s --format=%%(trailers:only,unfold):\n%s", trailers)
			t.Logf("PR title received by disposable forge: fix: preserve repair rationale\nPR body received by disposable forge:\n%s", body)
		})
	}
}

// The accepted summary limit is 4096 bytes. This 4091-byte input used to fit
// that limit but fail once flattened into the prefixed commit subject.
func TestReviewFixLongSummaryLiveJourney(t *testing.T) {
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME": "Victor I", "GIT_AUTHOR_EMAIL": "vifarinde@gmail.com",
		"GIT_COMMITTER_NAME": "Victor I", "GIT_COMMITTER_EMAIL": "vifarinde@gmail.com",
	} {
		t.Setenv(key, value)
	}
	summary := strings.TrimSpace(strings.Repeat("rebind stale receipts ", 186))
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, summary)})
	cfg := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("isolated init: %v\n%s", err, out)
	} else {
		t.Logf("no-mistakes init:\n%s", out)
	}
	const branch = "feature/long-review-summary"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch); err != nil {
		t.Fatalf("isolated gate push: %v\n%s", err, out)
	} else {
		t.Logf("git push no-mistakes %s:\n%s", branch, out)
	}
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	if out, err := h.Run("axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("isolated Review approval: %v\n%s", err, out)
	} else {
		t.Logf("no-mistakes axi respond --action approve:\n%s", out)
	}
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run = %s, error=%v", run.Status, run.Error)
	}
	message := liveStoredCommitMessage(t, h, h.UpstreamDir, "refs/heads/"+branch)
	const wantSubject = "no-mistakes(review): rebind stale receipts rebind stale receipts rebind stale receipts rebind stale receipts rebind"
	want := wantSubject + "\n\n" + summary + "\n\nRound findings:\n- feature.txt: unsafe value needs validation\n"
	if !bytes.Equal(message, []byte(want)) {
		t.Fatalf("long summary commit = %q, want %q", message, want)
	}
	t.Logf("CLI journey: no-mistakes init; git push no-mistakes %s; isolated Review approval; local publication completed", branch)
	t.Logf("git show -s --format=%%B refs/heads/%s (4091-byte summary retained):\n%s", branch, message)
}

// The documented byte bound must reject a 4097-byte report even though the
// Review subject could be shortened. The fixer edits before returning it, so
// retaining the edit or publishing any repair would violate the contract.
func TestReviewFixOversizedSummaryLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, strings.Repeat("x", 4097))})
	cfg := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(data), "  review: 0\n", "  review: 1\n", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("isolated init: %v\n%s", err, out)
	} else {
		t.Logf("no-mistakes init:\n%s", out)
	}
	const branch = "feature/oversized-review-summary"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch); err != nil {
		t.Fatalf("isolated gate push: %v\n%s", err, out)
	} else {
		t.Logf("git push no-mistakes %s:\n%s", branch, out)
	}
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "commit summary must not exceed 4096 bytes") {
		t.Fatalf("oversized report outcome = %s, error=%v", run.Status, deref(run.Error))
	}
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
	content, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
	if err != nil || string(content) != "unsafe\n" {
		t.Fatalf("rejected repair was not discarded: %q, %v", content, err)
	}
	head, err := h.runGit(t.Context(), worktree, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != submitted {
		t.Fatalf("rejected repair changed HEAD: %q, %v", head, err)
	}
	status, err := h.runGit(t.Context(), worktree, "status", "--porcelain")
	if err != nil || len(status) != 0 {
		t.Fatalf("rejected repair left edits or index changes: %q, %v", status, err)
	}
	if out, err := h.runGit(t.Context(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("rejected repair was published: %s", out)
	}
	out, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("observe rejected run: %v\n%s", err, out)
	}
	t.Logf("no-mistakes axi status:\n%s", out)
	t.Logf("Rejected 4097-byte report: %s\nRestored feature.txt: %q\nHEAD unchanged at %s; worktree and index clean; disposable upstream has no branch.", *run.Error, content, submitted)
}
