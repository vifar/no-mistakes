package gitlab

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const gitlabFacts42 = `{"iid":42,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/42","state":"opened","source_project_id":9,"target_project_id":7,"source_branch":"feature","target_branch":"develop","sha":"0123456789abcdef0123456789abcdef01234567"}`
const gitlabFacts43 = `{"iid":43,"web_url":"https://gitlab.example.com/group/project/-/merge_requests/43","state":"opened","source_project_id":9,"target_project_id":7,"source_branch":"feature","target_branch":"main","sha":"1123456789abcdef0123456789abcdef01234567"}`

func gitlabFactsHost(responses map[string]gitlabTestResponse) *Host {
	responses["glab api --hostname gitlab.example.com --method GET projects/0"] = gitlabTestResponse{stdout: `{"id":0,"path_with_namespace":"other/fork"}`}
	responses["glab api --hostname gitlab.example.com --method GET projects/9"] = gitlabTestResponse{stdout: `{"id":9,"path_with_namespace":"other/fork"}`}
	responses["glab api --hostname gitlab.example.com --method GET projects/7"] = gitlabTestResponse{stdout: `{"id":7,"path_with_namespace":"group/project"}`}
	return New(gitlabTestCmdFactory(responses), nil, "gitlab.example.com", "group/project")
}

func TestReadPRFactsBindsExactIdentityAndSource(t *testing.T) {
	h := gitlabFactsHost(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: gitlabFacts42},
	})
	facts, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42", URL: "https://gitlab.example.com/group/project/-/merge_requests/42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.PR.URL != "https://gitlab.example.com/group/project/-/merge_requests/42" || facts.SourceRepository != "other/fork" || facts.SourceBranch != "feature" || facts.HeadSHA != "0123456789abcdef0123456789abcdef01234567" || facts.BaseBranch != "develop" || facts.State != scm.PRStateOpen {
		t.Fatalf("incomplete GitLab facts: %+v", facts)
	}
}

func TestFindOpenPRFactsReturnsEveryPaginatedCandidate(t *testing.T) {
	h := gitlabFactsHost(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: `[ ` + gitlabFacts42 + ` ]` + "\n" + `[ ` + gitlabFacts43 + ` ]`},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), "Other/Fork", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].PR.Number != "42" || facts[1].PR.Number != "43" {
		t.Fatalf("candidate list = %+v", facts)
	}
}

func TestGitLabPRFactsFailClosed(t *testing.T) {
	wantErrors := map[string]string{"missing source": "incomplete source, head, or target", "missing head": "incomplete source, head, or target", "bad URL": "inconsistent URL and IID", "unknown state": "unsupported state", "wrong project identity": "incomplete or inconsistent identity"}
	for _, tc := range []struct{ name, body, sourceProject string }{
		{"missing source", strings.Replace(gitlabFacts42, `"source_project_id":9,`, "", 1), ""},
		{"missing head", strings.Replace(gitlabFacts42, `"sha":"0123456789abcdef0123456789abcdef01234567"`, `"sha":""`, 1), ""},
		{"bad URL", strings.Replace(gitlabFacts42, "/merge_requests/42", "/merge_requests/41", 1), ""},
		{"unknown state", strings.Replace(gitlabFacts42, `"state":"opened"`, `"state":"locked"`, 1), ""},
		{"wrong project identity", gitlabFacts42, `{"id":8,"path_with_namespace":"other/fork"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := gitlabFactsHost(map[string]gitlabTestResponse{"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: gitlabFacts42}})
			if facts, err := valid.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.PR.Number != "42" {
				t.Fatalf("valid baseline = %+v, %v", facts, err)
			}
			h := gitlabFactsHost(map[string]gitlabTestResponse{
				"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: tc.body},
			})
			if tc.sourceProject != "" {
				h = New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
					"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: tc.body},
					"glab api --hostname gitlab.example.com --method GET projects/9":                                 {stdout: tc.sourceProject},
					"glab api --hostname gitlab.example.com --method GET projects/7":                                 {stdout: `{"id":7,"path_with_namespace":"group/project"}`},
				}), nil, "gitlab.example.com", "group/project")
			}
			if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil || !strings.Contains(err.Error(), wantErrors[tc.name]) {
				t.Fatalf("rejection = %v, want %q", err, wantErrors[tc.name])
			}
		})
	}
}

func TestFindOpenPRFactsRejectsCorruptLaterPage(t *testing.T) {
	valid := gitlabFactsHost(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: `[` + gitlabFacts42 + `]` + "\n[]"},
	})
	if facts, err := valid.FindOpenPRFacts(context.Background(), "other/fork", "feature"); err != nil || len(facts) != 1 {
		t.Fatalf("valid pages = %+v, %v", facts, err)
	}
	h := gitlabFactsHost(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: `[` + gitlabFacts42 + `]` + "\n" + `[broken`},
	})
	if _, err := h.FindOpenPRFacts(context.Background(), "other/fork", "feature"); err == nil {
		t.Fatal("FindOpenPRFacts accepted a partial paginated result")
	}
}

func TestFindOpenPRFactsRejectsDuplicateAndIncompleteCandidates(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"duplicate MR", `[` + gitlabFacts42 + `,` + gitlabFacts42 + `]`},
		{"missing source project", `[` + strings.Replace(gitlabFacts42, `"source_project_id":9,`, "", 1) + `]`},
		{"non-open response", `[` + strings.Replace(gitlabFacts42, `"state":"opened"`, `"state":"merged"`, 1) + `]`},
		{"null page", `null`},
		{"non-array page", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := gitlabFactsHost(map[string]gitlabTestResponse{"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: `[` + gitlabFacts42 + `]`}})
			if facts, err := valid.FindOpenPRFacts(context.Background(), "other/fork", "feature"); err != nil || len(facts) != 1 {
				t.Fatalf("valid baseline = %+v, %v", facts, err)
			}
			h := gitlabFactsHost(map[string]gitlabTestResponse{
				"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: tc.body},
			})
			want := ""
			if tc.name == "missing source project" {
				want = "incomplete source, head, or target"
			}
			if tc.name == "duplicate MR" {
				want = "duplicate"
			}
			if tc.name == "non-open response" {
				want = "non-open"
			}
			if _, err := h.FindOpenPRFacts(context.Background(), "other/fork", "feature"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("rejection = %v, want %q", err, want)
			}
		})
	}
}

func TestFindOpenPRFactsAllowsCompleteEmptyList(t *testing.T) {
	h := gitlabFactsHost(map[string]gitlabTestResponse{
		"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests -f state=opened -f source_branch=feature -f per_page=100 --paginate": {stdout: `[]`},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), "other/fork", "feature")
	if err != nil || len(facts) != 0 {
		t.Fatalf("complete empty list = %+v, %v", facts, err)
	}
}

func TestReadPRFactsMergedResultMatchesGitLabMergeMethod(t *testing.T) {
	const mergeSHA = "abcdef0123456789abcdef0123456789abcdef01"
	const squashSHA = "1123456789abcdef0123456789abcdef01234567"
	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	merged := strings.Replace(gitlabFacts42, `"state":"opened"`, `"state":"merged"`, 1)
	for _, tc := range []struct {
		name, fields, wantSHA string
		wantError             bool
	}{
		{"merge commit", fmt.Sprintf(`"merge_commit_sha":%q,"squash_commit_sha":%q,`, mergeSHA, squashSHA), mergeSHA, false},
		{"squash fast-forward", fmt.Sprintf(`"merge_commit_sha":null,"squash_commit_sha":%q,`, squashSHA), squashSHA, false},
		{"plain fast-forward", `"merge_commit_sha":null,"squash_commit_sha":null,`, headSHA, false},
		{"missing optional commits", "", headSHA, false},
		{"empty optional commits", `"merge_commit_sha":"","squash_commit_sha":"",`, headSHA, false},
		{"short merge commit with valid squash", fmt.Sprintf(`"merge_commit_sha":"abcdef","squash_commit_sha":%q,`, squashSHA), "", true},
		{"nonhex merge commit", fmt.Sprintf(`"merge_commit_sha":%q,`, strings.Repeat("z", 40)), "", true},
		{"whitespace merge commit", fmt.Sprintf(`"merge_commit_sha":%q,`, " "+mergeSHA), "", true},
		{"short squash commit", `"merge_commit_sha":null,"squash_commit_sha":"abcdef",`, "", true},
		{"malformed squash with valid merge", fmt.Sprintf(`"merge_commit_sha":%q,"squash_commit_sha":"bad",`, mergeSHA), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := gitlabFactsHost(map[string]gitlabTestResponse{"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: strings.Replace(merged, `{`, `{"merge_commit_sha":"`+mergeSHA+`",`, 1)}})
			if facts, err := baseline.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.MergeCommitSHA != mergeSHA {
				t.Fatalf("valid merged baseline = %+v, %v", facts, err)
			}
			payload := strings.Replace(merged, `{`, `{`+tc.fields, 1)
			host := gitlabFactsHost(map[string]gitlabTestResponse{
				"glab api --hostname gitlab.example.com --method GET projects/group%2Fproject/merge_requests/42": {stdout: payload},
			})
			facts, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted malformed merged result: %+v", facts)
				}
				return
			}
			if err != nil || facts.State != scm.PRStateMerged || facts.MergeCommitSHA != tc.wantSHA || facts.HeadSHA != headSHA || facts.PR.HeadSHA != headSHA || facts.SourceRepository != "other/fork" || facts.BaseBranch != "develop" {
				t.Fatalf("merged facts = %+v, %v; want merge result %q and unchanged source head", facts, err, tc.wantSHA)
			}
		})
	}
}
