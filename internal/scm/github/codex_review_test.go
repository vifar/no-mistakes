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
		{"submitted after request", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`, `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"` + head + `","state":"COMMENTED","submitted_at":"2026-10-21T12:01:00Z"}]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(codexRequestTestResponses(head, map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":            {stdout: tc.reviews},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":          {stdout: `[[{"id":7,"user":{"login":"owner"},"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}]]`},
				"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp": {stdout: tc.reactions},
			})), nil, "", "test/repo")
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

func TestCodexCompletionHeadBinding(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	const request = `{"id":7,"user":{"login":"owner"},"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`
	for _, tc := range []struct {
		name, reviews, summary string
		wantPending            bool
	}{
		{"old review", `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","state":"COMMENTED","submitted_at":"2026-10-21T12:01:00Z"}]]`, "", true},
		{"old summary", `[[]]`, ",{\"user\":{\"login\":\"chatgpt-codex-connector[bot]\"},\"body\":\"**Reviewed commit:** `bbbbbbbbbb`\",\"created_at\":\"2026-10-21T12:01:00Z\"}", true},
		{"current summary", `[[]]`, ",{\"user\":{\"login\":\"chatgpt-codex-connector[bot]\"},\"body\":\"**Reviewed commit:** `0123456789`\",\"created_at\":\"2026-10-21T12:01:00Z\"}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(codexRequestTestResponses(head, map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":            {stdout: tc.reviews},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":          {stdout: "[[" + request + tc.summary + "]]"},
				"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp": {stdout: `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`},
			})), nil, "", "test/repo")
			bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
			pending, err := host.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if err != nil || pending != tc.wantPending {
				t.Fatalf("pending=%v err=%v; want %v: only current-head completion can settle this request", pending, err, tc.wantPending)
			}
		})
	}
}

func TestCodexRequestSettlementSurvivesHeadAdvance(t *testing.T) {
	const old = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, summary := range []bool{false, true} {
		for _, tc := range []struct {
			name, requestedAt, completedAt string
			pending                        bool
		}{
			{"historical request remains settled", "12:00:00", "12:01:00", false},
			{"late historical completion still settles its request", "12:00:00", "12:03:00", false},
			{"new head request remains pending", "12:02:30", "12:03:00", true},
		} {
			t.Run(fmt.Sprintf("summary=%v/%s", summary, tc.name), func(t *testing.T) {
				reviews, extra := `[[]]`, ""
				stamp := "2026-10-21T" + tc.completedAt + "Z"
				if summary {
					extra = fmt.Sprintf(",{\"user\":{\"login\":\"chatgpt-codex-connector[bot]\"},\"body\":\"**Reviewed commit:** `bbbbbbbbbb`\",\"created_at\":%q}", stamp)
				} else {
					reviews = fmt.Sprintf(`[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":%q,"state":"COMMENTED","body":"P1 finding","submitted_at":%q}]]`, old, stamp)
				}
				comments := fmt.Sprintf(`[[{"id":7,"body":"@codex review","created_at":"2026-10-21T%sZ"}%s]]`, tc.requestedAt, extra)
				host := New(githubTestCmdFactory(map[string]githubTestResponse{
					"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":                               {stdout: reviews},
					"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":                             {stdout: comments},
					"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp":                    {stdout: `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`},
					"gh api --method GET repos/test/repo/pulls/42":                                                          {stdout: fmt.Sprintf(`{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","head":{"ref":"topic","sha":%q,"repo":{"full_name":"fork/repo"}},"base":{"ref":"main"}}`, head)},
					"gh api --method GET repos/fork/repo/activity?ref=refs%2Fheads%2Ftopic&per_page=100 --paginate --slurp": {stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":"0000000000000000000000000000000000000000","after":%q,"pushed_at":"2026-10-21T11:00:00Z"},{"ref":"refs/heads/topic","before":%q,"after":%q,"pushed_at":"2026-10-21T12:02:00Z"}]]`, old, old, head)},
				}), nil, "", "test/repo")
				bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
				pending, err := host.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
				if err != nil || pending != tc.pending {
					t.Fatalf("pending=%v err=%v; want %v", pending, err, tc.pending)
				}
			})
		}
	}
}

func codexRequestTestResponses(head string, overrides map[string]githubTestResponse) map[string]githubTestResponse {
	responses := map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42":                                                          {stdout: fmt.Sprintf(`{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","head":{"ref":"topic","sha":%q,"repo":{"full_name":"fork/repo"}},"base":{"ref":"main"}}`, head)},
		"gh api --method GET repos/fork/repo/activity?ref=refs%2Fheads%2Ftopic&per_page=100 --paginate --slurp": {stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":"0000000000000000000000000000000000000000","after":%q,"pushed_at":"2026-10-21T11:00:00Z"}]]`, head)},
	}
	for key, response := range overrides {
		responses[key] = response
	}
	return responses
}

func TestCodexRequestAttributionFailsClosed(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const old = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const activity = "gh api --method GET repos/fork/repo/activity?ref=refs%2Fheads%2Ftopic&per_page=100 --paginate --slurp"
	for _, tc := range []struct {
		name      string
		history   githubTestResponse
		wantError bool
	}{
		{"missing history", githubTestResponse{stdout: `[[]]`}, false},
		{"read denied", githubTestResponse{stderr: "HTTP 403", code: 1}, true},
		{"malformed page", githubTestResponse{stdout: `[{}]`}, true},
		{"wrong branch", githubTestResponse{stdout: fmt.Sprintf(`[[{"ref":"refs/heads/other","before":%q,"after":%q,"pushed_at":"2026-10-21T11:00:00Z"}]]`, old, head)}, true},
		{"wrong head", githubTestResponse{stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":%q,"after":%q,"pushed_at":"2026-10-21T11:00:00Z"}]]`, head, old)}, true},
		{"push races request", githubTestResponse{stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":%q,"after":%q,"pushed_at":"2026-10-21T12:00:00Z"}]]`, old, head)}, false},
		{"history starts after request", githubTestResponse{stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":%q,"after":%q,"pushed_at":"2026-10-21T12:00:30Z"}]]`, old, head)}, false},
		{"disconnected pushes", githubTestResponse{stdout: fmt.Sprintf(`[[{"ref":"refs/heads/topic","before":%q,"after":%q,"pushed_at":"2026-10-21T11:00:00Z"},{"ref":"refs/heads/topic","before":"cccccccccccccccccccccccccccccccccccccccc","after":%q,"pushed_at":"2026-10-21T11:30:00Z"}]]`, head, old, head)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := codexRequestTestResponses(head, map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":            {stdout: fmt.Sprintf(`[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":%q,"state":"COMMENTED","submitted_at":"2026-10-21T12:01:00Z"}]]`, head)},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":          {stdout: `[[{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}]]`},
				"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp": {stdout: `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`},
				activity: tc.history,
			})
			host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
			bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
			pending, err := host.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if (err != nil) != tc.wantError || err == nil && !pending {
				t.Fatalf("pending=%v err=%v; want error=%v and never readiness", pending, err, tc.wantError)
			}
		})
	}
}

func TestCodexSettledRequestDoesNotSettleAnotherRequest(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, summary := range []bool{false, true} {
		t.Run(fmt.Sprint(summary), func(t *testing.T) {
			reviews, extra := `[[]]`, ""
			if summary {
				extra = ",{\"user\":{\"login\":\"chatgpt-codex-connector[bot]\"},\"body\":\"**Reviewed commit:** `aaaaaaaaaa`\",\"created_at\":\"2026-10-21T12:01:00Z\"}"
			} else {
				reviews = `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"` + head + `","state":"COMMENTED","submitted_at":"2026-10-21T12:01:00Z"}]]`
			}
			responses := codexRequestTestResponses(head, map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42/reviews --paginate --slurp":            {stdout: reviews},
				"gh api --method GET repos/test/repo/issues/42/comments --paginate --slurp":          {stdout: `[[{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"},{"id":8,"body":"@codex review","created_at":"2026-10-21T12:02:00Z"}` + extra + `]]`},
				"gh api --method GET repos/test/repo/issues/comments/7/reactions --paginate --slurp": {stdout: `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`},
				"gh api --method GET repos/test/repo/issues/comments/8/reactions --paginate --slurp": {stdout: `[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`},
			})
			host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
			bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
			pending, err := host.ReviewBotPending(context.Background(), &scm.PR{Number: "42", HeadSHA: head}, bot)
			if err != nil || !pending {
				t.Fatalf("pending=%v err=%v; the later acknowledged request is still outstanding", pending, err)
			}
		})
	}
}
