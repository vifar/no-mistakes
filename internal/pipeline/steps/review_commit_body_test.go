package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #98: inspect the real Git body, separately from the subject, after
// executing the same repair and commit path the review step uses.
func TestReviewFixCommitBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, summary, wantBody, wantSubject string
	}{
		{"rationale", "Reject stale receipts so approval stays bound to the head", "Reject stale receipts so approval stays bound to the head", "Reject stale receipts so approval stays bound to the head"},
		{"misleading wording", "no changes applied: checked formatting", "no changes applied: checked formatting", "no changes applied: checked formatting"},
		{"reserved no-change wording", "no changes applied", "Fixer summary: no changes applied", "no changes applied"},
		{"multiline disclosure", "Bind receipts to HEAD\n\nRed first: stale receipt accepted before fix.\nGreen: focused regression passed.", "Red first: stale receipt accepted before fix.\nGreen: focused regression passed.", "Bind receipts to HEAD Red first: stale receipt accepted before fix. Green: focused regression passed"},
		{"missing", "", "body: unavailable - fixer returned no summary", "address review findings"},
		{"whitespace", " \n\t", "body: unavailable - fixer returned no summary", "address review findings"},
		{"quoted attribution", "Guard receipts\n> Co-Authored-By: Claude <noreply@anthropic.com>\nGenerated with Codex\nClaude-Session: abc\nCo-Authored-By: Alex Example <alex@example.com>", "Guard receipts", "Guard receipts Co-Authored-By: Alex Example <alex@example.com>"},
		{"only attribution", "> Co-Authored-By: Claude <noreply@anthropic.com>", "body: unavailable - summary contained only attribution", "address review findings"},
		{"model attribution only", "Assisted-by: gpt-5.5", "body: unavailable - summary contained only attribution", "address review findings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			output, err := json.Marshal(map[string]string{"summary": tc.summary, "verification": "passed"})
			if err != nil {
				t.Fatal(err)
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				writeAgentChange(t, dir)
				return &agent.Result{Output: output}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"r1","file":"receipt.go","description":"Stale receipt could approve a different head"}]}`
			gotSummary, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "address review findings", RequireVerification: true})
			if err != nil {
				t.Fatal(err)
			}
			body := gitCmd(t, dir, "show", "-s", "--format=%b", "HEAD")
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if !strings.Contains(body, "- receipt.go: Stale receipt could approve a different head") {
				t.Errorf("round finding missing from body: %q", body)
			}
			if got := lastCommitMessage(t, dir); got != "no-mistakes(review): "+tc.wantSubject {
				t.Errorf("subject = %q", got)
			}
			if !strings.Contains(body, gotSummary) || gotSummary == "changes applied" {
				t.Errorf("round summary %q is not the body summary: %q", gotSummary, body)
			}
			for _, forbidden := range []string{"Co-Authored-By: Claude", "Generated with", "Claude-Session"} {
				if strings.Contains(gitCmd(t, dir, "show", "-s", "--format=%B"), forbidden) {
					t.Errorf("commit contains attribution %q", forbidden)
				}
			}
			if tc.name == "quoted attribution" && !strings.Contains(body, "Co-Authored-By: Alex Example <alex@example.com>") {
				t.Errorf("human attribution lost: %q", body)
			}
		})
	}
}

func TestReviewFixCommitBodyBoundsFindings(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	var findings []Finding
	for i := 0; i < 30; i++ {
		findings = append(findings, Finding{ID: fmt.Sprint(i), File: "receipt.go", Description: fmt.Sprintf("finding %02d %s", i, strings.Repeat("界", 400))})
	}
	raw, _ := json.Marshal(Findings{Items: findings})
	sctx.PreviousFindings = string(raw)
	writeAgentChange(t, dir)
	if err := commitAgentFixes(sctx, types.StepReview, "Bind approval receipts", "fallback"); err != nil {
		t.Fatal(err)
	}
	body := gitCmd(t, dir, "show", "-s", "--format=%b")
	if !strings.Contains(body, "finding 00") || !strings.Contains(body, "25 more findings omitted") {
		t.Fatalf("unreadable findings summary: %q", body)
	}
	if strings.Contains(body, "finding 05") || utf8.RuneCountInString(body) > 1600 || !utf8.ValidString(body) {
		t.Fatalf("findings were dumped or damaged: %q", body)
	}
}

func TestReviewFixCommitBodySanitizesFindingsAndTrailers(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Config.Commit.Trailers = []string{"Co-Authored-By: Claude <noreply@anthropic.com>", "Assisted-by: {{.Model}}"}
	sctx.PreviousFindings = `{"findings":[{"description":"Guard approval\n> Co-Authored-By: Codex <ai@example.com>\nGenerated with Claude"}]}`
	writeAgentChange(t, dir)
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "Guard receipts\nCo-Authored-By: Alex Example <alex@example.com>", "fallback", &agent.Result{Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	message := gitCmd(t, dir, "show", "-s", "--format=%B")
	if strings.Contains(message, "Co-Authored-By: Claude") || strings.Contains(message, "Co-Authored-By: Codex") || strings.Contains(message, "Generated with") {
		t.Fatalf("attribution leaked: %q", message)
	}
	if !strings.Contains(message, "Co-Authored-By: Alex Example <alex@example.com>") {
		t.Fatalf("human trailer lost: %q", message)
	}
}

func TestReviewFixCommitR2ConfiguredAttribution(t *testing.T) {
	t.Parallel()
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const human = "Co-Authored-By: Claude Smith <claude@example.com>"
	const humanWithAIEmail = "Co-Authored-By: Alex Example <codex@example.com>"
	attribution := []string{
		"Generated with Codex",
		"> Generated with Claude",
		"Claude-Session: abc",
		"Codex-Session: abc",
		"Assisted-by: pi:local",
		"Co-Authored-By: Claude <noreply@anthropic.com>",
		"> Co-Authored-By: Codex <ai@example.com>",
		`"Co-Authored-By: ChatGPT <ai@example.com>"`,
		"`Assisted-by: GitHub Copilot`",
		"Co-Authored-By: Gemini <ai@example.com>",
		"Assisted-by: Grok",
		"Co-Authored-By: no-mistakes codex <noreply@example.com>",
		"Assisted-by: no-mistakes:claude:local",
		"Assisted-by: gpt-5.5",
		"Co-Authored-By: gpt-5.5 <model@example.com>",
		"> Reviewed-By: gpt-5.5",
		`"Reviewed-By: gpt-5.5"`,
		"Build-Agent: private-identifier",
		"Co-Authored-By: configured-identity <noreply@example.com>",
		"Co-Authored-By: acp:cursor:model <noreply@example.com>",
	}
	noise := strings.Join(attribution, "\n")
	cleanSummary := "Guard receipts\n\n" + disclosure + "\n\n" + human + "\n" + humanWithAIEmail
	summary := cleanSummary + "\n" + noise
	dir, base, head := setupGitRepo(t)
	output, err := json.Marshal(map[string]string{"summary": summary, "verification": "passed"})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "pi", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAgentChange(t, dir)
		return &agent.Result{Output: output, Model: "gpt-5.5"}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.Config.Commit.Trailers = []string{
		"Assisted-by: {{.Agent}}:local",
		"Assisted-by: {{.Model}}",
		"Co-Authored-By: {{.Model}} <model@example.com>",
		"Reviewed-By: {{.Model}}",
		"Build-Agent: private-identifier",
		"Co-Authored-By: configured-identity <noreply@example.com>",
		"Codex-Session: configured",
		"Co-Authored-By: acp:cursor:model <noreply@example.com>",
	}
	findings, err := json.Marshal(Findings{Items: []Finding{
		{File: "receipt.go\n" + noise, Description: disclosure + "\n" + noise},
		{Description: human},
		{File: humanWithAIEmail, Description: "Keep the human attribution"},
		{File: disclosure, Description: "Keep ordinary prose"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sctx.PreviousFindings = string(findings)
	gotSummary, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "address review findings", RequireVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	if gotSummary != cleanSummary {
		t.Errorf("persisted summary = %q, want %q", gotSummary, cleanSummary)
	}
	assertPersistedReviewFixSummary(t, sctx.DB, sctx.Run.ID, gotSummary, cleanSummary)
	wantBody := cleanSummary + "\n\nRound findings:\n- receipt.go: " + disclosure + "\n- " + human + "\n- " + humanWithAIEmail + ": Keep the human attribution\n- " + disclosure + ": Keep ordinary prose"
	if body := gitCmd(t, dir, "show", "-s", "--format=%b"); body != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
	wantSubject := "no-mistakes(review): Guard receipts " + disclosure + " " + human + " " + humanWithAIEmail
	if subject := lastCommitMessage(t, dir); subject != wantSubject {
		t.Errorf("subject = %q, want %q", subject, wantSubject)
	}
	if trailers := gitTrailers(t, dir); trailers != "" {
		t.Errorf("trailers = %q, want no configured attribution", trailers)
	}
}

func TestReviewFixCommitR1DisclosureAndHumanAttribution(t *testing.T) {
	t.Parallel()
	assertUnconfiguredReviewAttributionFiltered(t, "🤖 Generated with [Claude Code](https://claude.ai/code)\nGenerated with Codex\n> Co-Authored-By: Claude <noreply@anthropic.com>\nAssisted-by: pi:local\nCodex-Session: abc")
}

func TestReviewFixCommitFoldedAttribution(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{
		"Co-Authored-By: Claude\n <noreply@anthropic.com>",
		"Co-Authored-By: gpt-5.5\n\t<model@example.com>",
		"> \"Co-Authored-By: Claude\n <noreply@anthropic.com>\"",
	} {
		t.Run(entry, func(t *testing.T) {
			assertUnconfiguredReviewAttributionFiltered(t, entry)
		})
	}
}

func TestReviewFixCommitR1QuotedDisplayNames(t *testing.T) {
	t.Parallel()
	t.Run("Claude tool mailbox", func(t *testing.T) {
		assertUnconfiguredReviewAttributionFiltered(t, `Co-Authored-By: "Claude" <noreply@anthropic.com>`)
	})
	for _, identity := range []string{
		"Claude Code", "Codex", "ChatGPT", "OpenAI", "Anthropic", "Copilot", "GitHub Copilot",
		"Gemini", "Grok", "pi", "omp", "OpenCode", "Antigravity", "RovoDev", "Cursor", "Devin",
		"acp:cursor", "acp:cursor:model", "acp:custom-target", "no-mistakes", "no-mistakes codex",
		"gpt-5.5", "o3", "gemini-2.5-pro", "grok-4", "claude-opus-4-6",
	} {
		t.Run(identity, func(t *testing.T) {
			assertUnconfiguredReviewAttributionFiltered(t, `Co-Authored-By: "`+identity+`" <model@example.com>`)
		})
	}
}

func TestReviewFixCommitR3UnconfiguredAIIdentities(t *testing.T) {
	t.Parallel()
	t.Run("model coauthor with default empty trailers", func(t *testing.T) {
		assertUnconfiguredReviewAttributionFiltered(t, "Co-Authored-By: gpt-5.5 <model@example.com>")
	})
	t.Run("quoted model coauthor with default empty trailers", func(t *testing.T) {
		assertUnconfiguredReviewAttributionFiltered(t, `> "Co-Authored-By: gpt-5.5 <model@example.com>"`)
	})
	for _, identity := range []string{
		"claude", "codex", "grok", "rovodev", "opencode", "pi", "omp", "copilot",
		"cursor", "devin", "antigravity", "acp:cursor", "acp:devin", "acp:custom-target",
	} {
		t.Run(identity, func(t *testing.T) {
			mailbox := "model@example.com"
			if identity == "claude" {
				mailbox = "noreply@anthropic.com"
			}
			noise := "Co-Authored-By: " + identity + " <" + mailbox + ">\n" +
				"Assisted-by: " + identity + ":model\n" +
				"> " + identity + "-Session: abc"
			assertUnconfiguredReviewAttributionFiltered(t, noise)
		})
	}
}

func assertUnconfiguredReviewAttributionFiltered(t *testing.T, attribution string) {
	t.Helper()
	const human = "Co-Authored-By: Claude Smith <claude@example.com>"
	const quotedHuman = `Co-Authored-By: "Claude Smith" <claude@example.com>`
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const personalClaude = "Co-Authored-By: Claude <claude@example.com>"
	const foldedHuman = "Co-Authored-By: Claude\n <claude@example.com>"
	const recorder = "Generated with the repository recorder"
	const cleanSummary = "guard unsafe value\n\n" + disclosure + "\n\n" + human + "\n" + quotedHuman + "\n" + personalClaude + "\n" + foldedHuman + "\n" + recorder
	dir, base, head := setupGitRepo(t)
	output, err := json.Marshal(map[string]string{"summary": cleanSummary + "\n" + attribution, "verification": "passed"})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAgentChange(t, dir)
		return &agent.Result{Output: output}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	if len(sctx.Config.Commit.Trailers) != 0 {
		t.Fatal("identity classification requires default empty commit trailers")
	}
	findings, err := json.Marshal(Findings{Items: []Finding{
		{File: "receipt.go\n" + attribution, Description: disclosure + "\n" + human + "\n" + quotedHuman + "\n" + attribution},
		{File: quotedHuman, Description: disclosure},
		{File: foldedHuman, Description: recorder},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = string(findings)
	summary, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "address review findings", RequireVerification: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary != cleanSummary {
		t.Errorf("returned summary = %q, want %q", summary, cleanSummary)
	}
	wantSubject := "no-mistakes(review): guard unsafe value " + disclosure + " " + human + " " + quotedHuman + " " + personalClaude + " Co-Authored-By: Claude <claude@example.com> " + recorder
	if subject := lastCommitMessage(t, dir); subject != wantSubject {
		t.Errorf("subject = %q, want %q", subject, wantSubject)
	}
	wantBody := cleanSummary + "\n\nRound findings:\n- receipt.go: " + disclosure + " " + human + " " + quotedHuman + "\n- " + quotedHuman + ": " + disclosure + "\n- " + personalClaude + ": " + recorder
	if body := gitCmd(t, dir, "show", "-s", "--format=%b"); body != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
	assertPersistedReviewFixSummary(t, sctx.DB, sctx.Run.ID, summary, cleanSummary)
}

func assertPersistedReviewFixSummary(t *testing.T, database *db.DB, runID, summary, want string) {
	t.Helper()
	step, err := database.InsertStepResult(runID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(step.ID, 1, "auto_fix", nil, &summary, 0); err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].FixSummary == nil || *rounds[0].FixSummary != want {
		t.Fatalf("persisted round did not retain the cleaned summary: %+v", rounds)
	}
}

func TestOtherFixCommitBodiesUnchanged(t *testing.T) {
	t.Parallel()
	for _, step := range []types.StepName{types.StepDocument, types.StepLint} {
		t.Run(string(step), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			writeAgentChange(t, dir)
			if err := commitAgentFixes(sctx, step, "repair widget", "fallback"); err != nil {
				t.Fatal(err)
			}
			if body := gitCmd(t, dir, "show", "-s", "--format=%b"); body != "" {
				t.Fatalf("%s body changed: %q", step, body)
			}
		})
	}
}

func TestReviewAttributionEntriesPreserveUnchangedText(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"Co-Authored-By: Claude\n <claude@example.com>",
		"Co-Authored-By: Claude Smith\n\t<claude@example.com>",
		"Disclosure: fixtures were generated with the repository recorder.\n  Keep this continuation.\n\nKeep this paragraph.",
		"Ordinary prose\n  with indentation.\n\nGenerated with the repository recorder",
	} {
		if got := stripReviewAIAttribution(text, nil); got != text {
			t.Errorf("text changed: %q, want %q", got, text)
		}
	}
	if got := stripReviewAIAttribution("Build-Agent: private\n identifier\n\nKeep rationale", []string{"Build-Agent: private identifier"}); got != "Keep rationale" {
		t.Errorf("configured folded entry survived: %q", got)
	}
}
