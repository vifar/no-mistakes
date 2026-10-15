package github

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	protectionCmd = "gh api --method GET repos/test/repo/branches/main/protection/required_status_checks"
	rulesCmd      = "gh api --method GET repos/test/repo/rules/branches/main?per_page=100 --paginate --slurp"
)

func requiredContexts(t *testing.T, responses map[string]githubTestResponse) ([]string, error) {
	t.Helper()
	host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
	required, err := host.GetRequiredChecks(context.Background(), &scm.PR{Number: "1"}, "main")
	var contexts []string
	for _, r := range required {
		contexts = append(contexts, r.Context)
	}
	sort.Strings(contexts)
	return contexts, err
}

func TestGetRequiredCheckContextsUnionsProtectionAndRules(t *testing.T) {
	t.Parallel()
	contexts, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stdout: `{"strict":true,"contexts":["build","lint"],"checks":[{"context":"build","app_id":1},{"context":"test (ubuntu)","app_id":-1}]}`},
		rulesCmd: {stdout: `[[{"type":"pull_request","parameters":{}},` +
			`{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test (ubuntu)"},{"context":"ruleset-only"}]}}]]`},
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

func TestGetRequiredCheckContextsRejectsPartialSources(t *testing.T) {
	for _, source := range []string{protectionCmd, rulesCmd} {
		for _, failure := range []string{"HTTP 403", "HTTP 404", "HTTP 502"} {
			t.Run(source+failure, func(t *testing.T) {
				responses := map[string]githubTestResponse{
					protectionCmd: {stdout: `{"contexts":["build"]}`},
					rulesCmd:      {stdout: `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]]`},
				}
				responses[source] = githubTestResponse{stderr: failure, code: 1}
				host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
				var logs []string
				scope := scm.NewCheckScope(host, scm.CheckScopePolicy{}, "main", func(m string) { logs = append(logs, m) })
				scoped := scope.Apply(context.Background(), &scm.PR{}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "optional", Bucket: scm.CheckBucketFail}})
				if len(scoped.Relevant) != 2 {
					t.Fatalf("partial source omitted failures: %+v", scoped)
				}
				label := "branch protection"
				if source == rulesCmd {
					label = "branch rules"
				}
				if len(logs) != 1 || !strings.Contains(logs[0], label) || !strings.Contains(logs[0], "evaluating all checks") {
					t.Fatalf("logs = %v", logs)
				}
			})
		}
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
		rulesCmd:      {stdout: `[[]]`},
	})
	if err == nil || errors.Is(err, scm.ErrRequiredChecksUnavailable) {
		t.Fatalf("error = %v, want a read failure that is not ErrRequiredChecksUnavailable", err)
	}
}

func TestGetRequiredCheckContextsEscapesBranchSegments(t *testing.T) {
	t.Parallel()
	if got := escapeBranchPath("release/1.0 beta"); got != "release%2F1.0%20beta" {
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

func TestGetRequiredChecksPaginatesRules(t *testing.T) {
	t.Parallel()
	contexts, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stdout: `{"contexts":[],"checks":[]}`},
		// The required_status_checks rule is only on the second page.
		rulesCmd: {stdout: `[[{"type":"pull_request","parameters":{}},{"type":"deletion"}],` +
			`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"page-two"}]}}]]`},
	})
	if err != nil || !reflect.DeepEqual(contexts, []string{"page-two"}) {
		t.Fatalf("contexts = %v, err = %v, want the rule from page 2", contexts, err)
	}
}

func TestGetRequiredChecksKeepsApplicationIdentity(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		protectionCmd: {stdout: `{"contexts":["build","legacy"],"checks":[{"context":"build","app_id":15368},{"context":"any","app_id":-1},{"context":"null-app","app_id":null}]}`},
		rulesCmd: {stdout: `[[{"type":"required_status_checks","parameters":{"required_status_checks":` +
			`[{"context":"ruleset","integration_id":42},{"context":"unbound-rule"}]}}]]`},
	}), nil, "", "test/repo")
	got, err := host.GetRequiredChecks(context.Background(), &scm.PR{Number: "1"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []scm.RequiredCheck{
		{Context: "build", AppID: 15368},
		{Context: "any"},
		{Context: "null-app"},
		{Context: "legacy"},
		{Context: "ruleset", AppID: 42},
		{Context: "unbound-rule"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required checks = %+v, want %+v (the legacy contexts list must not unbind build)", got, want)
	}
}

func TestGetChecksReadsProducingAppID(t *testing.T) {
	t.Parallel()
	nodes := `[{"__typename":"CheckRun","databaseId":1,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"slug":"github-actions","databaseId":15368}}},` +
		`{"__typename":"StatusContext","id":"s1","context":"legacy","state":"SUCCESS"}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse(nodes)},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int64{}
	for _, c := range checks {
		byName[c.Name] = c.AppID
	}
	if byName["build"] != 15368 || byName["legacy"] != 0 || len(byName) != 2 {
		t.Fatalf("app IDs = %v, want build=15368 and the legacy status unattributed", byName)
	}
}

func TestGetRequiredChecksExcludesPipelineStatusFromEverySource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		protection string
		rules      string
	}{
		{"legacy protection", `{"contexts":["build","no-mistakes"]}`, `[[]]`},
		{"app-bound protection", `{"contexts":["build","no-mistakes"],"checks":[{"context":"no-mistakes","app_id":7}]}`, `[[]]`},
		{"ruleset", `{"contexts":["build"]}`, `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"no-mistakes","integration_id":7}]}}]]`},
		{"whitespace", `{"contexts":["build"," no-mistakes "]}`, `[[]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contexts, err := requiredContexts(t, map[string]githubTestResponse{
				protectionCmd: {stdout: tc.protection},
				rulesCmd:      {stdout: tc.rules},
			})
			if err != nil || !reflect.DeepEqual(contexts, []string{"build"}) {
				t.Fatalf("requirements = %v, %v; want only build", contexts, err)
			}
		})
	}
}

func TestGetRequiredChecksFallsBackWhenRulesetsHoldUnmodeledCheckRules(t *testing.T) {
	t.Parallel()
	for _, ruleType := range []string{"workflows", "code_scanning", "required_deployments"} {
		t.Run(ruleType, func(t *testing.T) {
			t.Parallel()
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				protectionCmd: {stdout: `{"contexts":[],"checks":[]}`},
				// The ruleset requires "build" and also gates on a rule whose
				// checks are not named, so "build" alone is an incomplete set.
				rulesCmd: {stdout: `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"}]}},` +
					`{"type":"` + ruleType + `","parameters":{}}]]`},
			}), nil, "", "test/repo")
			var logs []string
			scope := scm.NewCheckScope(host, scm.CheckScopePolicy{}, "main", func(m string) { logs = append(logs, m) })
			scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "h"}, []scm.Check{
				{Name: "build", Bucket: scm.CheckBucketPass},
				{Name: "required-workflow / job", Bucket: scm.CheckBucketFail},
			})
			if scoped.Ready() || len(scoped.Relevant) != 2 || len(scoped.Informational) != 0 {
				t.Fatalf("a failing check from a required workflow must block through the all-checks fallback: %+v", scoped)
			}
			if len(logs) != 1 || !strings.Contains(logs[0], ruleType) || !strings.Contains(logs[0], "evaluating all checks") {
				t.Fatalf("logs = %v, want one fallback line naming %s", logs, ruleType)
			}
		})
	}
}

func TestGetRequiredChecksIgnoresUnrelatedRuleTypes(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		protectionCmd: {stdout: `{"contexts":[],"checks":[]}`},
		rulesCmd: {stdout: `[[{"type":"deletion"},{"type":"pull_request","parameters":{}},{"type":"non_fast_forward"},` +
			`{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build"}]}}]]`},
	}), nil, "", "test/repo")
	var logs []string
	scope := scm.NewCheckScope(host, scm.CheckScopePolicy{}, "main", func(m string) { logs = append(logs, m) })
	scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "h"}, []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketPass},
		{Name: "optional", Bucket: scm.CheckBucketFail},
	})
	if !scoped.Ready() || len(scoped.Informational) != 1 {
		t.Fatalf("unrelated rule types must not trigger the fallback: %+v (logs %v)", scoped, logs)
	}
}
