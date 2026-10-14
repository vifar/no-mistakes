package azuredevops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const azFactsSHA = "0123456789abcdef0123456789abcdef01234567"

const azFacts42 = `{"pullRequestId":42,"status":"active","sourceRefName":"refs/heads/feature","targetRefName":"refs/heads/develop","lastMergeSourceCommit":{"commitId":"` + azFactsSHA + `"},"repository":{"name":"myrepo","project":{"name":"myproject"}}}`

func TestReadPRFactsBindsExactRecordedIdentityAndLiveBase(t *testing.T) {
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	pr := &scm.PR{Number: "42", URL: webPRURL(testOrg, testProject, testRepo, "", "42"), BaseBranch: "main"}
	facts, err := h.ReadPRFacts(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if facts.PR.Number != "42" || facts.PR.URL != pr.URL || facts.State != scm.PRStateOpen ||
		facts.SourceRepository != webPRURL(testOrg, testProject, testRepo, "", "") ||
		facts.SourceBranch != "feature" || facts.HeadSHA != azFactsSHA || facts.PR.HeadSHA != azFactsSHA ||
		facts.BaseBranch != "develop" || facts.PR.BaseBranch != "develop" {
		t.Fatalf("incomplete Azure PR facts: %+v", facts)
	}
}

func azFactsResponses() map[string]azdoTestResponse {
	return map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	}
}

func TestReadPRFactsRejectsIncompleteAndInconsistentData(t *testing.T) {
	for _, tc := range []struct{ name, body, wantError string }{
		{"wrong number", strings.Replace(azFacts42, `"pullRequestId":42`, `"pullRequestId":43`, 1), "different PR identity"},
		{"missing head", strings.Replace(azFacts42, `"commitId":"`+azFactsSHA+`"`, `"commitId":""`, 1), "lacks a full source commit SHA"},
		{"short head", strings.Replace(azFacts42, azFactsSHA, "1234", 1), "lacks a full source commit SHA"},
		{"bad source ref", strings.Replace(azFacts42, "refs/heads/feature", "refs/tags/feature", 1), `invalid Azure branch ref "refs/tags/feature"`},
		{"bad target ref", strings.Replace(azFacts42, "refs/heads/develop", "develop", 1), `invalid Azure branch ref "develop"`},
		{"unknown status", strings.Replace(azFacts42, `"status":"active"`, `"status":"unknown"`, 1), `invalid status "unknown"`},
		{"foreign target", strings.Replace(azFacts42, `"name":"myrepo"`, `"name":"other"`, 1), `repository metadata name "other" does not match configured repository "myrepo"`},
		{"bad fork", strings.Replace(azFacts42, `"repository":`, `"forkSource":{"repository":{}},"repository":`, 1), "fork source lacks repository project or name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := azFactsResponses()
			h := newTestHost(responses)
			pr := &scm.PR{Number: "42"}
			facts, err := h.ReadPRFacts(context.Background(), pr)
			if err != nil || facts.PR.Number != "42" || facts.HeadSHA != azFactsSHA || facts.BaseBranch != "develop" {
				t.Fatalf("valid PR facts = %+v, %v", facts, err)
			}
			responses["az repos pr show --id 42 --organization "+testOrg+" --output json"] = azdoTestResponse{stdout: tc.body}
			if _, err := h.ReadPRFacts(context.Background(), pr); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ReadPRFacts error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestReadPRFactsRejectsMismatchedPersistedURL(t *testing.T) {
	h := newTestHost(azFactsResponses())
	pr := &scm.PR{Number: "42", URL: webPRURL(testOrg, testProject, testRepo, "", "42")}
	facts, err := h.ReadPRFacts(context.Background(), pr)
	if err != nil || facts.PR.URL != pr.URL || facts.HeadSHA != azFactsSHA {
		t.Fatalf("valid recorded PR = %+v, %v", facts, err)
	}
	pr.URL = webPRURL(testOrg, testProject, testRepo, "", "43")
	if _, err := h.ReadPRFacts(context.Background(), pr); err == nil || !strings.Contains(err.Error(), "recorded URL does not match PR identity") {
		t.Fatalf("ReadPRFacts error = %v, want recorded URL identity rejection", err)
	}
}

func TestFindOpenPRFactsReturnsEveryPagedCandidateAndAcceptsPushURL(t *testing.T) {
	const pageSize = 100
	first := make([]string, pageSize)
	for i := range first {
		first[i] = strings.Replace(azFacts42, `"pullRequestId":42`, fmt.Sprintf(`"pullRequestId":%d`, i+1), 1)
	}
	last := strings.Replace(azFacts42, `"pullRequestId":42`, `"pullRequestId":101`, 1)
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json":   {stdout: "[" + strings.Join(first, ",") + "]"},
		"az repos pr list --source-branch feature --status active --top 100 --skip 100 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: "[" + last + "]"},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json":                                      {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), "git@ssh.dev.azure.com:v3/MyOrg/MyProject/MyRepo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 101 || facts[0].PR.Number != "1" || facts[100].PR.Number != "101" || facts[100].BaseBranch != "develop" {
		t.Fatalf("candidate list = %+v", facts)
	}
}

func TestReadPRFactsRejectsStalePRMergeSnapshot(t *testing.T) {
	valid := newTestHost(azFactsResponses())
	if facts, err := valid.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.HeadSHA != azFactsSHA {
		t.Fatalf("valid live head = %+v, %v", facts, err)
	}
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"1123456789abcdef0123456789abcdef01234567"}]`},
	})
	if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
		t.Fatal("ReadPRFacts accepted stale lastMergeSourceCommit")
	}
}

func TestReadPRFactsUsesForkSourceRepository(t *testing.T) {
	fork := strings.Replace(azFacts42, `"repository":`, `"forkSource":{"repository":{"name":"fork","project":{"name":"team"}}},"repository":`, 1)
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                  {stdout: fork},
		"az repos ref list --filter heads/feature --organization " + testOrg + " --project team --repository fork --output json": {stdout: `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
	})
	facts, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "https://dev.azure.com/myorg/team/_git/fork" {
		t.Fatalf("source repository = %q", facts.SourceRepository)
	}
}

func TestReadPRFactsRejectsMissingOrAmbiguousLiveSourceRef(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", `[]`},
		{"prefix only", `[{"name":"refs/heads/feature/other","objectId":"` + azFactsSHA + `"}]`},
		{"duplicate", `[{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"},{"name":"refs/heads/feature","objectId":"` + azFactsSHA + `"}]`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := newTestHost(azFactsResponses())
			if facts, err := valid.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.HeadSHA != azFactsSHA {
				t.Fatalf("valid live ref = %+v, %v", facts, err)
			}
			h := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json":                                                                             {stdout: azFacts42},
				"az repos ref list --filter heads/feature --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: tc.body},
			})
			if _, err := h.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err == nil {
				t.Fatal("ReadPRFacts accepted missing or ambiguous live source ref")
			}
		})
	}
}

func TestFindOpenPRFactsRejectsCorruptAndDuplicateCandidates(t *testing.T) {
	command := "az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json"
	other := strings.Replace(azFacts42, `"pullRequestId":42`, `"pullRequestId":43`, 1)
	for _, tc := range []struct{ name, body, wantError string }{
		{"null", "null", "invalid array"},
		{"object", azFacts42, "invalid array"},
		{"missing head", "[" + strings.Replace(azFacts42, azFactsSHA, "", 1) + "]", "lacks a full source commit SHA"},
		{"wrong source branch", "[" + strings.Replace(azFacts42, "refs/heads/feature", "refs/heads/other", 1) + "]", "non-open or mismatched source branch"},
		{"duplicate", "[" + azFacts42 + "," + strings.Replace(other, `"pullRequestId":43`, `"pullRequestId":42`, 1) + "]", "duplicate PR 42"},
		{"foreign target", "[" + azFacts42 + "," + strings.Replace(other, `"name":"myrepo"`, `"name":"other"`, 1) + "]", `repository metadata name "other" does not match configured repository "myrepo"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := azFactsResponses()
			responses[command] = azdoTestResponse{stdout: "[" + azFacts42 + "," + other + "]"}
			h := newTestHost(responses)
			source := webPRURL(testOrg, testProject, testRepo, "", "")
			facts, err := h.FindOpenPRFacts(context.Background(), source, "feature")
			if err != nil || len(facts) != 2 || facts[0].PR.Number != "42" || facts[1].PR.Number != "43" || facts[0].HeadSHA != azFactsSHA || facts[1].HeadSHA != azFactsSHA {
				t.Fatalf("valid candidate list = %+v, %v", facts, err)
			}
			responses[command] = azdoTestResponse{stdout: tc.body}
			if _, err := h.FindOpenPRFacts(context.Background(), source, "feature"); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("FindOpenPRFacts error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestFindOpenPRFactsAcceptsProvenEmptyAndRejectsForeignSource(t *testing.T) {
	h := newTestHost(map[string]azdoTestResponse{
		"az repos pr list --source-branch feature --status active --top 100 --skip 0 --organization " + testOrg + " --project " + testProject + " --repository " + testRepo + " --output json": {stdout: "[]"},
	})
	facts, err := h.FindOpenPRFacts(context.Background(), webPRURL(testOrg, testProject, testRepo, "", ""), "feature")
	if err != nil || len(facts) != 0 {
		t.Fatalf("empty result = %+v, %v", facts, err)
	}
	if _, err := h.FindOpenPRFacts(context.Background(), "https://dev.azure.com/other/myproject/_git/myrepo", "feature"); err == nil {
		t.Fatal("FindOpenPRFacts accepted foreign source repository")
	}
}

func TestReadPRFactsMergedCommitFailsClosed(t *testing.T) {
	const mergeSHA = "abcdef0123456789abcdef0123456789abcdef01"
	merged := strings.Replace(azFacts42, `"status":"active"`, `"status":"completed"`, 1)
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
			baseline := newTestHost(map[string]azdoTestResponse{"az repos pr show --id 42 --organization " + testOrg + " --output json": {stdout: strings.Replace(merged, `{`, `{"lastMergeCommit":{"commitId":"`+mergeSHA+`"},`, 1)}})
			if facts, err := baseline.ReadPRFacts(context.Background(), &scm.PR{Number: "42"}); err != nil || facts.MergeCommitSHA != mergeSHA {
				t.Fatalf("valid merged baseline = %+v, %v", facts, err)
			}
			payload := merged
			if !tc.omit {
				payload = strings.Replace(payload, `{`, `{`+fmt.Sprintf(`"lastMergeCommit":{"commitId":%q},`, tc.sha), 1)
			}
			host := newTestHost(map[string]azdoTestResponse{
				"az repos pr show --id 42 --organization " + testOrg + " --output json": {stdout: payload},
			})
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
