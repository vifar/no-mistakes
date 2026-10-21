//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func configurableFixCommitScenario(t *testing.T, fixSummary string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configurable-fix-commit-scenario.yaml")
	content := strings.Replace(`actions:
  - match: "Investigate previous review findings"
    text: "fixed unsafe value"
    edits:
      - path: "feature.txt"
        old: "unsafe"
        new: "safe"
    structured:
      summary: "FIX_SUMMARY"
      verification: passed
  - match: "Review the code changes and return structured findings"
    text: "review found an issue"
    structured:
      findings:
        - id: "configurable-fix-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "unsafe value needs validation"
          action: auto-fix
      summary: "found one issue"
      risk_level: medium
      risk_rationale: "the unsafe value needs a guard"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: guard unsafe value"
      body: "configurable fix commit journey"
`, "FIX_SUMMARY", fixSummary, 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write configurable fix commit scenario: %v", err)
	}
	return path
}

func TestConfigurableFixCommitMessageJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, "guard unsafe value\\n\\nCo-Authored-By: Claude <noreply@anthropic.com>\\nClaude-Session: abc")})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += "commit:\n  fix_message: 'chore(global-{{.Step}}): {{.Summary}}'\n"
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/configurable-fix-commit"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.CommitChange(branch, ".no-mistakes.yaml", `ignore_patterns:
  - '*.generated.go'
  - 'vendor/**'
allow_repo_commands: true
commit:
  fix_message: 'fix(repo-{{.Step}}): {{.Summary}}'
`, "configure pipeline fix commits")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
	}
	subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
	const want = "fix(repo-review): guard unsafe value"
	if len(subjects) == 0 || subjects[0] != want {
		t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], want, subjects)
	}
	for _, subject := range subjects {
		if strings.HasPrefix(subject, "chore(global-") {
			t.Fatalf("global template won over repository template: %q", subject)
		}
	}
	body, err := h.runGit(context.Background(), h.UpstreamDir, "show", "-s", "--format=%b", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read published review-fix body: %v", err)
	}
	if !strings.Contains(string(body), "guard unsafe value") || !strings.Contains(string(body), "- feature.txt: unsafe value needs validation") {
		t.Fatalf("published review-fix body lost its summary or finding: %q", body)
	}
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
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
		if round.IsFixRound() && round.FixSummary != nil && *round.FixSummary == "guard unsafe value" {
			found = true
		}
	}
	if !found {
		t.Fatal("published review-fix summary was not preserved in step_rounds.fix_summary")
	}

	t.Logf("global config: commit.fix_message = %q", "chore(global-{{.Step}}): {{.Summary}}")
	t.Logf("repository config (higher precedence): commit.fix_message = %q", "fix(repo-{{.Step}}): {{.Summary}}")
	t.Logf("pipeline status: %s", run.Status)
	t.Logf("completed pipeline upstream commit subjects:\n%s", strings.TrimSpace(string(log)))
}

func TestFixCommitTrailersJourney(t *testing.T) {
	// Exercise folded raw attribution before the summary becomes a subject.
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, "guard unsafe value\\n\\nCo-Authored-By: Claude\\n <noreply@anthropic.com>\\nClaude-Session: abc\\nCodex-Session: def\\nAssisted-by: pi:local\\nGenerated with Claude Code")})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += "commit:\n  trailers:\n    - 'Assisted-by: no-mistakes:{{.Agent}}:{{.Model}}'\n    - 'Assisted-by: pi:local'\n    - 'Codex-Session: abc'\n    - 'Co-Authored-By: Claude Smith <claude@example.com>'\n    - 'Co-Authored-By: Claude <claude@example.com>'\n    - 'Signed-off-by: Some Person <person@example.com>'\n    - 'Disclosure: fixtures were generated with the repository recorder.'\n    - 'Context: Discuss Claude-Session: abc and Assisted-by: pi:local handling'\n"
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/fix-commit-trailers"
	const submittedMessage = "add unsafe feature\n\nKeep this submitted body.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", submittedMessage)
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)

	worktree := paths.WithRoot(h.NMHome).WorktreeDir(gated.RepoID, gated.ID)
	beforePush, err := h.runGit(context.Background(), worktree, "log", "-1", "--format=%B")
	if err != nil {
		t.Fatalf("read correction before publication: %v\n%s", err, beforePush)
	}
	if got, want := strings.TrimSpace(string(beforePush)), "no-mistakes(review): guard unsafe value\n\nguard unsafe value\n\nRound findings:\n- feature.txt: unsafe value needs validation"; got != want {
		t.Fatalf("correction before publication = %q, want %q", got, want)
	}
	if out, err := h.runGit(context.Background(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("branch was published before the review gate was released: %s", out)
	}
	t.Logf("correction already sanitized while review is parked, before publication:\n%s", beforePush)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	// git's own trailer parser reads the pushed commits, so this checks that
	// human attribution survives as a trailer and AI attribution is absent.
	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s%x00%(trailers:only,unfold)%x00", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commits: %v\n%s", err, log)
	}
	fields := strings.Split(string(log), "\x00")
	var fixTrailers, userTrailers []string
	for i := 0; i+1 < len(fields); i += 2 {
		subject := strings.TrimSpace(fields[i])
		trailers := strings.TrimSpace(fields[i+1])
		switch subject {
		case "no-mistakes(review): guard unsafe value":
			fixTrailers = append(fixTrailers, trailers)
		case "add unsafe feature":
			userTrailers = append(userTrailers, trailers)
		}
	}
	if len(fixTrailers) != 1 || fixTrailers[0] != "" {
		t.Fatalf("review fix commit trailers = %q, want none", fixTrailers)
	}
	if len(userTrailers) != 1 || userTrailers[0] != "Co-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted" {
		t.Fatalf("user commit trailers = %q, want the user's commit left untouched", userTrailers)
	}
	if out, err := h.runGit(context.Background(), h.UpstreamDir, "merge-base", "--is-ancestor", submitted, "refs/heads/"+branch); err != nil {
		t.Fatalf("submitted commit was rewritten: %v\n%s", err, out)
	}
	submittedBody, err := h.runGit(context.Background(), h.UpstreamDir, "log", "-1", "--format=%B", submitted)
	if err != nil || strings.TrimSpace(string(submittedBody)) != submittedMessage {
		t.Fatalf("submitted message changed: %v\n%s", err, submittedBody)
	}
	t.Logf("submitted commit %s remains an ancestor with its original message:\n%s", submitted, submittedBody)
	t.Logf("completed pipeline upstream commits and trailers:\n%s", strings.ReplaceAll(string(log), "\x00", " | "))
}

func TestGlobalBranchReplacementFixCommitJourney(t *testing.T) {
	const summary = "Preserve legacy drafts and batch status invariants"
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, summary)})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += `commit:
  branch_pattern: '^PROJ/([0-9]+)$'
  branch_replacement: 'PROJ-${1}'
  fix_message: '{{.Branch}}: {{.Summary}}'
`
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "PROJ/123"
	h.CommitChange(branch, ".no-mistakes.yaml", `ignore_patterns:
  - '*.generated.go'
  - 'vendor/**'
allow_repo_commands: true
commit:
  branch_replacement: 'WRONG-${1}'
`, "attempt repository branch replacement")
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
	}
	subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
	const want = "PROJ-123: Preserve legacy drafts and batch status invariants"
	if len(subjects) == 0 || subjects[0] != want {
		t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], want, subjects)
	}

	t.Logf("branch pushed through no-mistakes: %s", branch)
	t.Logf("machine-local replacement: %q", "PROJ-${1}")
	t.Logf("completed pipeline upstream commit subjects:\n%s", strings.TrimSpace(string(log)))
}

func TestPerRepositoryMachineLocalFixCommitJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, "guard unsafe value\\n\\nCo-Authored-By: Claude <noreply@anthropic.com>\\nClaude-Session: abc")})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += `repository_overrides:
  https://github.com/example/no-mistakes.git:
    commit:
      branch_pattern: '([A-Z]+-[0-9]+)'
      fix_message: '{{.Branch}}: {{.Summary}}'
`
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	const remote = "https://github.com/example/no-mistakes.git"
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	if out, err := h.runGit(context.Background(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
		t.Fatalf("set GitHub-shaped origin: %v\n%s", err, out)
	}
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-commit-override.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/PROJ-123-machine-local"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "-1", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
	}
	subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
	const want = "PROJ-123: guard unsafe value"
	if len(subjects) == 0 || subjects[0] != want {
		t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], want, subjects)
	}
}
