package github

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const testPRFactsJSON = `{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","merged":false,"head":{"ref":"feature","sha":"0123456789abcdef0123456789abcdef01234567","repo":{"full_name":"test/repo"}},"base":{"ref":"develop"}}`

func TestReadPRFactsBindsHeadSourceAndLiveBase(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: testPRFactsJSON},
	}), nil, "", "test/repo")

	facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.SourceRepository != "test/repo" || facts.SourceBranch != "feature" || facts.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || facts.BaseBranch != "develop" || facts.State != scm.PRStateOpen {
		t.Fatalf("incomplete PR facts: %+v", facts)
	}
}

func TestFindOpenPRFactsReturnsEveryCandidateForExactSource(t *testing.T) {
	second := strings.ReplaceAll(testPRFactsJSON, `"number":42`, `"number":43`)
	second = strings.ReplaceAll(second, "/pull/42", "/pull/43")
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {
			stdout: `[[` + testPRFactsJSON + `,` + second + `]]`,
		},
	}), nil, "", "test/repo")

	facts, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("FindOpenPRFacts returned %d candidates, want both for ambiguity detection", len(facts))
	}
}

func TestReadPRFactsRejectsWrongIdentityAndMissingHead(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: testPRFactsJSON},
	}), nil, "", "test/repo")
	if facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.PR.Number != "42" {
		t.Fatalf("valid identity = %+v, %v", facts, err)
	}
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/99"}); err == nil {
		t.Fatal("accepted a different recorded PR URL")
	}
	missingHead := strings.ReplaceAll(testPRFactsJSON, "0123456789abcdef0123456789abcdef01234567", "")
	host = New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls/42": {stdout: missingHead},
	}), nil, "", "test/repo")
	if _, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("accepted a PR without an exact head")
	}
}

func TestFindOpenPRFactsRejectsMalformedLaterPage(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {
			stdout: `[[` + testPRFactsJSON + `],null]`,
		},
	}), nil, "", "test/repo")
	valid := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {stdout: `[[` + testPRFactsJSON + `],[]]`},
	}), nil, "", "test/repo")
	if facts, err := valid.FindOpenPRFacts(context.Background(), "test/repo", "feature"); err != nil || len(facts) != 1 {
		t.Fatalf("valid pages = %+v, %v", facts, err)
	}
	if _, err := host.FindOpenPRFacts(context.Background(), "test/repo", "feature"); err == nil {
		t.Fatal("accepted an incomplete paginated PR list")
	}
}

func TestReadPRFactsMergedCommitFailsClosed(t *testing.T) {
	const mergeSHA = "abcdef0123456789abcdef0123456789abcdef01"
	merged := strings.Replace(testPRFactsJSON, `"state":"open","merged":false`, `"state":"closed","merged":true`, 1)
	for _, tc := range []struct {
		name, sha       string
		omit, wantError bool
	}{
		{"valid", mergeSHA, false, false},
		{"missing", "", true, true},
		{"empty", "", false, true},
		{"short", "abcdef", false, true},
		{"nonhex", strings.Repeat("z", 40), false, true},
		{"whitespace", " " + mergeSHA, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := New(githubTestCmdFactory(map[string]githubTestResponse{"gh api --method GET repos/test/repo/pulls/42": {stdout: strings.Replace(merged, `{`, `{"merge_commit_sha":"`+mergeSHA+`",`, 1)}}), nil, "", "test/repo")
			if facts, err := baseline.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.MergeCommitSHA != mergeSHA {
				t.Fatalf("valid merged baseline = %+v, %v", facts, err)
			}
			payload := merged
			if !tc.omit {
				payload = strings.Replace(payload, `{`, `{`+fmt.Sprintf(`"merge_commit_sha":%q,`, tc.sha), 1)
			}
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh api --method GET repos/test/repo/pulls/42": {stdout: payload},
			}), nil, "", "test/repo")
			facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted merged PR without a valid merge commit: %+v", facts)
				}
				return
			}
			if err != nil || facts.State != scm.PRStateMerged || facts.MergeCommitSHA != mergeSHA {
				t.Fatalf("merged facts = %+v, %v", facts, err)
			}
		})
	}
}

func TestPRFactsBindsAPIHostAndImmutableCoordinates(t *testing.T) {
	url := "https://ghe.example.test/owner/repo/pull/42"
	body := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","draft":true,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"full_name":"owner/repo"}}}`, url, strings.Repeat("a", 40), strings.Repeat("b", 40))
	host := New(githubTestCmdFactory(map[string]githubTestResponse{"gh api repos/owner/repo/pulls/42 --hostname ghe.example.test": {stdout: body}}), nil, "ghe.example.test", "owner/repo")
	got, err := host.GetPRFacts(context.Background(), &scm.PR{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != scm.PRStateOpen || !got.Draft || got.SourceBranch != "feature" || got.TargetBranch != "main" || got.HeadSHA != strings.Repeat("a", 40) {
		t.Fatalf("facts: %+v", got)
	}
	for _, bad := range []string{`{`, strings.Replace(body, `"number":42`, `"number":43`, 1), strings.Replace(body, url, "https://ghe.example.test/owner/other/pull/42", 1)} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{"gh api repos/owner/repo/pulls/42 --hostname ghe.example.test": {stdout: bad}}), nil, "ghe.example.test", "owner/repo")
		if _, err := host.GetPRFacts(context.Background(), &scm.PR{URL: url}); err == nil {
			t.Fatal("malformed or wrong PR identity accepted")
		}
	}
}
