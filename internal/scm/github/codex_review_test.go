package github

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCodexUnresolvedAndResolvedThreads(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(fmt.Sprint(resolved), func(t *testing.T) {
			response := fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":%v,"comments":{"nodes":[{"databaseId":42,"body":"![P1 Badge](https://img.shields.io/badge/P1-orange) Fix the lost update","path":"internal/store.go","line":73,"author":{"login":"chatgpt-codex-connector[bot]"}}]}},{"isResolved":false,"comments":{"nodes":[{"body":"ordinary app","author":{"login":"other-app[bot]"}}]}}],"pageInfo":{}}}}}}`, resolved)
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api graphql -f query=" + reviewThreadsQuery + " -F owner=test -F name=repo -F number=42": {stdout: response},
			}), nil, "", "test/repo")
			got, err := host.GetReviewComments(context.Background(), &scm.PR{Number: "42"})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if resolved {
				want = 0
			}
			if len(got) != want {
				t.Fatalf("comments=%+v; want %d", got, want)
			}
			if !resolved && (got[0].Path != "internal/store.go" || got[0].Line != 73) {
				t.Fatalf("lost anchor: %+v", got)
			}
		})
	}
}

// Readiness needs completion evidence from review activity, rather than a check
// run Codex never creates. No signal from an unrelated app may block readiness.
func TestCodexReviewCompletion(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, reviews, comments string
		pending                 bool
	}{
		{"pending", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"` + head + `","state":"PENDING"}]]`, `[[]]`, true},
		{"submitted", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"` + head + `","state":"COMMENTED","submitted_at":"2026-10-21T12:00:00Z"}]]`, `[[]]`, false},
		{"summary", `[[]]`, "[[{\"user\":{\"login\":\"chatgpt-codex-connector[bot]\"},\"body\":\"### Codex Review\\n**Reviewed commit:** `0123456789`\",\"created_at\":\"2026-10-21T12:00:00Z\"}]]", false},
		{"old_head", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"old","state":"PENDING"}]]`, `[[]]`, false},
		{"other_app", `[[{"user":{"login":"other-app[bot]"},"commit_id":"` + head + `","state":"PENDING"}]]`, `[[]]`, false},
		{"absent", `[[]]`, `[[]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":   {stdout: tc.reviews},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp": {stdout: tc.comments},
			}), nil, "", "test/repo")
			reader, ok := any(host).(interface {
				ReviewBotPending(context.Context, *scm.PR, scm.ReviewBot) (bool, error)
			})
			if !ok {
				t.Fatal("GitHub does not read review completion signals")
			}
			bot, ok := scm.ReviewBotForApp("chatgpt-codex-connector")
			if !ok {
				t.Fatal("Codex is not registered")
			}
			got, err := reader.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.pending {
				t.Fatalf("pending=%v want %v", got, tc.pending)
			}
			state, e := host.reviewBotState(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if e != nil {
				t.Fatal(e)
			}
			want := "absent"
			if tc.name == "summary" || tc.name == "submitted" {
				want = "settled"
			}
			if tc.pending {
				want = "pending"
			}
			if state != want {
				t.Fatalf("state=%s want %s", state, want)
			}
		})
	}
}

func TestCodexCompletionReadErrorsFailClosed(t *testing.T) {
	host := New(githubTestCmdFactory(nil), nil, "", "test/repo")
	reader, ok := any(host).(interface {
		ReviewBotPending(context.Context, *scm.PR, scm.ReviewBot) (bool, error)
	})
	if !ok {
		t.Fatal("missing completion reader")
	}
	bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
	_, err := reader.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: strings.Repeat("a", 40)}, bot)
	if err == nil {
		t.Fatal("unreadable completion must fail closed")
	}
}

// The public integration acknowledges @codex review with an eyes reaction.
// An unfinished submitted review is not always visible to another account.
func TestCodexAcknowledgedReviewRequestCompletion(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, reactions, reviews string
		pending                  bool
	}{
		{"eyes", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`, `[[]]`, true},
		{"other app eyes", `[[{"user":{"login":"other-app[bot]"},"content":"eyes"}]]`, `[[]]`, false},
		{"clean reaction", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"},{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"+1"}]]`, `[[]]`, false},
		{"submitted after request", `[]`, `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"` + head + `","state":"COMMENTED","submitted_at":"2026-10-21T12:01:00Z"}]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":            {stdout: tc.reviews},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":          {stdout: `[[{"id":7,"user":{"login":"owner"},"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}]]`},
				"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp": {stdout: tc.reactions},
			}), nil, "", "test/repo")
			bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
			got, err := host.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.pending {
				t.Fatalf("pending=%v want %v", got, tc.pending)
			}
		})
	}
}
