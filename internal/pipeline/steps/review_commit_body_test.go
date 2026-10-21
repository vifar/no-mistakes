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
		{"multiline disclosure", "Bind receipts to HEAD\n\nRed first: stale receipt accepted before fix.\nGreen: focused regression passed.", "Red first: stale receipt accepted before fix.\nGreen: focused regression passed.", "Bind receipts to HEAD Red first: stale receipt accepted before fix. Green: focused regression passed"},
		{"missing", "", "body: unavailable - fixer returned no summary", "address review findings"},
		{"whitespace", " \n\t", "body: unavailable - fixer returned no summary", "address review findings"},
		{"quoted attribution", "Guard receipts\n> Co-Authored-By: Claude <noreply@anthropic.com>\nGenerated with Codex\nClaude-Session: abc\nCo-Authored-By: Alex Example <alex@example.com>", "Guard receipts", "Guard receipts Co-Authored-By: Alex Example <alex@example.com>"},
		{"only attribution", "> Co-Authored-By: Claude <noreply@anthropic.com>", "body: unavailable - summary contained only attribution", "address review findings"},
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
	sctx.Config.Commit.Trailers = []string{"Co-Authored-By: Claude <noreply@anthropic.com>", "Co-Authored-By: Alex Example <alex@example.com>"}
	sctx.PreviousFindings = `{"findings":[{"description":"Guard approval\n> Co-Authored-By: Codex <ai@example.com>\nGenerated with Claude"}]}`
	writeAgentChange(t, dir)
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "Guard receipts", "fallback", &agent.Result{}); err != nil {
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
