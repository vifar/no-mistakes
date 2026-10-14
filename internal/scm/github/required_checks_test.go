package github

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	protectionCmd = "gh api --method GET repos/test/repo/branches/main/protection/required_status_checks"
	rulesCmd      = "gh api --method GET repos/test/repo/rules/branches/main?per_page=100"
)

func requiredContexts(t *testing.T, responses map[string]githubTestResponse) ([]string, error) {
	t.Helper()
	host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
	contexts, err := host.GetRequiredCheckContexts(context.Background(), &scm.PR{Number: "1"}, "main")
	sort.Strings(contexts)
	return contexts, err
}

func TestGetRequiredCheckContextsUnionsProtectionAndRules(t *testing.T) {
	t.Parallel()
	contexts, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stdout: `{"strict":true,"contexts":["build","lint"],"checks":[{"context":"build","app_id":1},{"context":"test (ubuntu)","app_id":-1}]}`},
		rulesCmd: {stdout: `[{"type":"pull_request","parameters":{}},` +
			`{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test (ubuntu)"},{"context":"ruleset-only"}]}}]`},
	})
	if err != nil {
		t.Fatalf("GetRequiredCheckContexts() error = %v", err)
	}
	want := []string{"build", "lint", "ruleset-only", "test (ubuntu)"}
	if got := dedupe(contexts); !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %v, want union %v", contexts, want)
	}
}

func dedupe(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func TestGetRequiredCheckContextsUsesRulesWhenProtectionIsAbsent(t *testing.T) {
	t.Parallel()
	contexts, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stderr: "gh: Branch not protected (HTTP 404)", code: 1},
		rulesCmd:      {stdout: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`},
	})
	if err != nil || !reflect.DeepEqual(contexts, []string{"ci"}) {
		t.Fatalf("contexts = %v, err = %v, want [ci]", contexts, err)
	}
}

func TestGetRequiredCheckContextsUnavailableOn404And403(t *testing.T) {
	t.Parallel()
	for name, responses := range map[string]map[string]githubTestResponse{
		"both 404": {
			protectionCmd: {stderr: "gh: Not Found (HTTP 404)", code: 1},
			rulesCmd:      {stderr: "gh: Not Found (HTTP 404)", code: 1},
		},
		"protection 403 and empty rules": {
			protectionCmd: {stderr: "gh: Resource not accessible by integration (HTTP 403)", code: 1},
			rulesCmd:      {stderr: "gh: Resource not accessible by integration (HTTP 403)", code: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := requiredContexts(t, responses)
			if !errors.Is(err, scm.ErrRequiredChecksUnavailable) {
				t.Fatalf("error = %v, want ErrRequiredChecksUnavailable", err)
			}
		})
	}
}

func TestGetRequiredCheckContextsReportsOtherFailures(t *testing.T) {
	t.Parallel()
	_, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stderr: "gh: Bad Gateway (HTTP 502)", code: 1},
		rulesCmd:      {stdout: `[]`},
	})
	if err == nil || errors.Is(err, scm.ErrRequiredChecksUnavailable) {
		t.Fatalf("error = %v, want a read failure that is not ErrRequiredChecksUnavailable", err)
	}
}

func TestGetRequiredCheckContextsEscapesBranchSegments(t *testing.T) {
	t.Parallel()
	if got := escapeBranchPath("release/1.0 beta"); got != "release/1.0%20beta" {
		t.Fatalf("escapeBranchPath = %q", got)
	}
}

func TestGetChecksReadsWorkflowName(t *testing.T) {
	t.Parallel()
	nodes := `[{"__typename":"CheckRun","databaseId":1,"name":"approve","status":"COMPLETED","conclusion":"FAILURE","completedAt":"","startedAt":"","detailsUrl":"","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"Final Approval"}}}}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse(nodes)},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Workflow != "Final Approval" || checks[0].DisplayName() != "Final Approval / approve" {
		t.Fatalf("checks = %+v, want workflow name surfaced", checks)
	}
}
