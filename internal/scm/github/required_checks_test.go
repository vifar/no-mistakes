package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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

// GitHub answers the classic endpoint with a specific 404 when a branch is
// governed only by rulesets; the rules then decide alone.
func TestGetRequiredChecksReadsRulesetsWhenClassicProtectionIsAbsent(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"gh: Branch not protected (HTTP 404)", "gh: Required status checks not enabled (HTTP 404)"} {
		contexts, err := requiredContexts(t, map[string]githubTestResponse{
			protectionCmd: {stderr: message, code: 1},
			rulesCmd:      {stdout: `[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ruleset-only"}]}}]]`},
		})
		if err != nil || !reflect.DeepEqual(contexts, []string{"ruleset-only"}) {
			t.Fatalf("%s: contexts = %v, err = %v; want the ruleset requirement", message, contexts, err)
		}
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

func TestRequiredSelfStatusPreservesCheckRuns(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"protection", "ruleset"} {
		for _, tc := range []struct {
			name  string
			appID int64
			node  string
			ready bool
		}{
			{"bound failure", 7, `{"__typename":"CheckRun","databaseId":1,"name":"no-mistakes","status":"COMPLETED","conclusion":"FAILURE","checkSuite":{"app":{"databaseId":7}}}`, false},
			{"bound success", 7, `{"__typename":"CheckRun","databaseId":1,"name":"no-mistakes","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"databaseId":7}}}`, true},
			{"bound wrong app", 8, `{"__typename":"CheckRun","databaseId":1,"name":"no-mistakes","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"databaseId":7}}}`, false},
			{"bound missing", 7, "", false},
			{"unbound failure", 0, `{"__typename":"CheckRun","databaseId":1,"name":"no-mistakes","status":"COMPLETED","conclusion":"FAILURE","checkSuite":{"app":{"databaseId":7}}}`, false},
			{"unbound success", 0, `{"__typename":"CheckRun","databaseId":1,"name":"no-mistakes","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"databaseId":7}}}`, true},
			{"unbound self status only", 0, "", true},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				protection := `{"contexts":["build"]}`
				rules := `[[]]`
				if source == "protection" {
					protection = fmt.Sprintf(`{"contexts":["build","no-mistakes"],"checks":[{"context":"no-mistakes","app_id":%d}]}`, tc.appID)
				} else {
					rules = fmt.Sprintf(`[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"no-mistakes","integration_id":%d}]}}]]`, tc.appID)
				}
				nodes := `[{"__typename":"CheckRun","databaseId":2,"name":"build","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"StatusContext","id":"self","context":"no-mistakes","state":"PENDING"}`
				if tc.node != "" {
					nodes += "," + tc.node
				}
				nodes += "]"
				host := New(githubTestCmdFactory(map[string]githubTestResponse{
					protectionCmd: {stdout: protection},
					rulesCmd:      {stdout: rules},
					"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
					githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse(nodes)},
					"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":0,"workflow_runs":[]}]`},
				}), nil, "", "test/repo")
				pr := &scm.PR{Number: "5", HeadSHA: "abc123"}
				checks, err := host.GetChecks(context.Background(), pr)
				if err != nil {
					t.Fatal(err)
				}
				scope := scm.NewCheckScope(host, scm.CheckScopePolicy{}, "main", nil)
				scoped := scope.Apply(context.Background(), pr, checks)
				if scoped.Ready() != tc.ready {
					t.Fatalf("ready = %v, want %v: %+v", scoped.Ready(), tc.ready, scoped)
				}
				for _, check := range checks {
					if scope.Includes(context.Background(), pr, check) != (check.Name == "build" || tc.appID == 0 || check.AppID == tc.appID) {
						t.Fatalf("Includes disagrees with required app identity: %+v", check)
					}
				}
			})
		}
	}
}

func TestGetRequiredChecksRejectsObjectRulePages(t *testing.T) {
	t.Parallel()
	_, err := requiredContexts(t, map[string]githubTestResponse{
		protectionCmd: {stdout: `{"contexts":["build"]}`},
		rulesCmd:      {stdout: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`},
	})
	if err == nil {
		t.Fatal("object pages must fail the slurped array-page contract")
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

func TestGetChecksNewerWorkflowRunWithoutJobsSupersedesStaleJobResults(t *testing.T) {
	t.Parallel()
	job := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions","databaseId":15368},"workflowRun":{"workflow":{"name":"CI"}}}}`
	other := `{"__typename":"CheckRun","databaseId":6,"name":"docs","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/3/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"Docs"}}}}`
	runs := `[{"total_count":3,"workflow_runs":[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/1"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/2"},` +
		`{"id":3,"workflow_id":11,"name":"Docs","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/3"}]}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "," + other + "]")},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]scm.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if _, stale := byName["build"]; stale || len(byName) != 2 {
		t.Fatalf("the older attempt's passing job survived: %+v", checks)
	}
	if ci := byName["CI"]; ci.Bucket != scm.CheckBucketFail || !reflect.DeepEqual(ci.Supersedes, []scm.Check{{Name: "build", ProviderID: "github-check-run:5", Workflow: "CI", Kind: scm.CheckKindRun, App: "github-actions", AppID: 15368}}) {
		t.Fatalf("CI run = %+v, want a failure superseding build", ci)
	}
	if docs := byName["docs"]; docs.Bucket != scm.CheckBucketPass || len(docs.Supersedes) != 0 {
		t.Fatalf("an unrelated workflow's job was touched: %+v", docs)
	}
}

// A job named like its workflow (both "CI") must not be collapsed into the
// newer job-less workflow run before supersession records its identity: the
// workflow-level failure would lose the app that binds the required context.
func TestGetChecksSupersedesSameNameJobBeforeCollapsing(t *testing.T) {
	t.Parallel()
	job := `{"__typename":"CheckRun","databaseId":5,"name":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions","databaseId":15368},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/1"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/2"}]}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "]")},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want only the newer workflow-level failure", checks)
	}
	want := []scm.Check{{Name: "CI", ProviderID: "github-check-run:5", Workflow: "CI", Kind: scm.CheckKindRun, App: "github-actions", AppID: 15368}}
	if ci := checks[0]; ci.Bucket != scm.CheckBucketFail || !reflect.DeepEqual(ci.Supersedes, want) {
		t.Fatalf("CI run = %+v, want a failure superseding the app-bound CI job", ci)
	}
}

func checksForWorkflowRuns(t *testing.T, secondRunEvent string) map[string]scm.Check {
	return checksForWorkflowRunsAt(t, secondRunEvent, "completed", `"conclusion":"failure"`)
}

func checksForWorkflowRunsAt(t *testing.T, secondRunEvent, status, conclusion string) map[string]scm.Check {
	t.Helper()
	job := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/1"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"` + secondRunEvent + `","head_branch":"feature","status":"` + status + `",` + conclusion + `,"run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/2"}]}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "]")},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]scm.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	return byName
}

// A pull_request run that passed and a later workflow_dispatch run that failed
// are independent results: neither retries the other.
func TestSupersedeKeepsIndependentlyTriggeredRuns(t *testing.T) {
	t.Parallel()
	byName := checksForWorkflowRuns(t, "workflow_dispatch")
	if build, ok := byName["build"]; !ok || build.Bucket != scm.CheckBucketPass {
		t.Fatalf("the pull_request run's job was superseded by an independent run: %+v", byName)
	}
	if ci := byName["CI"]; ci.Bucket != scm.CheckBucketFail || len(ci.Supersedes) != 0 {
		t.Fatalf("the dispatched run = %+v, want a failure that supersedes nothing", ci)
	}
}

func TestSupersedeReplacesSameEventRetrigger(t *testing.T) {
	t.Parallel()
	byName := checksForWorkflowRuns(t, "pull_request")
	if _, stale := byName["build"]; stale || len(byName["CI"].Supersedes) != 1 {
		t.Fatalf("a same-event retrigger must supersede the older attempt: %+v", byName)
	}
}

func TestSupersedeNeedsAKnownEventAndDistinctRun(t *testing.T) {
	t.Parallel()
	h := New(nil, nil, "", "test/repo")
	job := scm.Check{Name: "build", Kind: scm.CheckKindRun, App: "github-actions", Workflow: "CI", Bucket: scm.CheckBucketPass,
		StartedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 10, 14, 10, 5, 0, 0, time.UTC),
		Link: "https://github.com/test/repo/actions/runs/7/job/1"}
	run := func(id int64, event, branch string, start time.Time) scm.Check {
		return scm.Check{Name: "CI", WorkflowRun: true, Kind: scm.CheckKindRun, WorkflowID: 10, RunID: id, WorkflowEvent: event, WorkflowBranch: branch,
			Bucket: scm.CheckBucketFail, StartedAt: start, Link: fmt.Sprintf("https://github.com/test/repo/actions/runs/%d", id)}
	}
	first := run(7, "push", "feature", time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	first.Bucket = scm.CheckBucketPass
	later := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		run  scm.Check
		want bool
	}{
		"unknown event":            {run(8, "", "feature", later), false},
		"different event":          {run(8, "pull_request", "feature", later), false},
		"same event, other branch": {run(8, "push", "other", later), false},
		"newer run, same event":    {run(8, "push", "feature", later), true},
	} {
		if name == "unknown event" {
			first.WorkflowEvent = ""
		} else {
			first.WorkflowEvent = "push"
		}
		got := h.appendUnrepresentedWorkflowRuns([]scm.Check{job}, []scm.Check{first, tc.run}, nil)
		superseded := true
		for _, c := range got {
			if !c.WorkflowRun && c.Name == "build" {
				superseded = false
			}
		}
		if superseded != tc.want {
			t.Errorf("%s: superseded = %v, want %v (%+v)", name, superseded, tc.want, got)
		}
	}
}

// A queued workflow_dispatch run has no job yet. It is not a retry of the
// pull_request run, so it inherits none of that run's required contexts: the
// PR run's job keeps satisfying "build" and the dispatch run stays informational.
func TestQueuedDispatchRunDoesNotInheritThePullRequestRunsRequirements(t *testing.T) {
	t.Parallel()
	byName := checksForWorkflowRunsAt(t, "workflow_dispatch", "queued", `"conclusion":null`)
	build, ok := byName["build"]
	ci := byName["CI"]
	if !ok || build.Bucket != scm.CheckBucketPass || ci.Bucket != scm.CheckBucketPending || len(ci.Supersedes) != 0 {
		t.Fatalf("checks = %+v, want the PR run's job kept and the queued dispatch run independent", byName)
	}
	host := &fakeRequiredHost{required: []scm.RequiredCheck{{Context: "build"}}}
	scoped := scm.NewCheckScope(host, scm.CheckScopePolicy{}, "main", nil).Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, []scm.Check{build, ci})
	if !scoped.Ready() || len(scoped.Relevant) != 1 || len(scoped.Informational) != 1 {
		t.Fatalf("scoped = %+v, want build relevant and ready, the queued dispatch run informational", scoped)
	}
}

type fakeRequiredHost struct {
	scm.Host
	required []scm.RequiredCheck
}

func (h *fakeRequiredHost) GetRequiredChecks(context.Context, *scm.PR, string) ([]scm.RequiredCheck, error) {
	return h.required, nil
}

func TestGetChecksSameRunRerunBlocksStalePass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status, conclusion, completed string
		bucket                              scm.CheckBucket
		workflow, ready                     bool
	}{
		// The stale job is retired; the unfinished attempt stands for it.
		{"queued rerun", "queued", "", "2026-10-14T09:00:00Z", scm.CheckBucketPending, true, false},
		{"queued rerun with stale conclusion", "queued", "success", "2026-10-14T09:00:00Z", scm.CheckBucketPending, true, false},
		{"running rerun", "in_progress", "", "2026-10-14T09:00:00Z", scm.CheckBucketPending, true, false},
		{"waiting rerun", "waiting", "", "2026-10-14T09:00:00Z", scm.CheckBucketPending, true, false},
		{"pending rerun", "pending", "", "2026-10-14T09:00:00Z", scm.CheckBucketPending, true, false},
		{"failed before jobs", "completed", "failure", "2026-10-14T09:00:00Z", scm.CheckBucketFail, true, false},
		{"new passing job", "completed", "success", "2026-10-14T11:00:00Z", scm.CheckBucketPass, false, true},
		{"new failing job", "completed", "failure", "2026-10-14T11:00:00Z", scm.CheckBucketFail, false, false},
		{"job completed at run start", "completed", "success", "2026-10-14T10:00:00Z", scm.CheckBucketPass, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := fmt.Sprintf(`{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","completedAt":%q,"detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"runAttempt":2,"workflow":{"name":"CI"}}}}`, tc.completed)
			if tc.name == "new failing job" {
				job = strings.Replace(job, `"conclusion":"SUCCESS"`, `"conclusion":"FAILURE"`, 1)
			}
			if tc.ready {
				oldJob := strings.ReplaceAll(job, tc.completed, "2026-10-14T09:00:00Z")
				oldJob = strings.Replace(oldJob, `"databaseId":5`, `"databaseId":4`, 1)
				oldJob = strings.Replace(oldJob, "/job/5", "/job/4", 1)
				job += "," + oldJob
			}

			// Attempt 2 owns job 5 only when the job ran in it, at or after the
			// attempt started; an earlier completion is attempt 1's result.
			current := attemptJobsResponse()
			if tc.completed >= "2026-10-14T10:00:00Z" {
				current = attemptJobsResponse(5)
			}
			runs := fmt.Sprintf(`[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"push","head_branch":"feature","status":%q,"conclusion":%q,"run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`, tc.status, tc.conclusion)
			host := New(githubTestCmdFactory(map[string]githubTestResponse{
				"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
				githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "]")},
				"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
				attemptJobsCommand(7, 2): current,
			}), nil, "", "test/repo")
			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != 1 || checks[0].Bucket != tc.bucket || checks[0].WorkflowRun != tc.workflow {
				t.Fatalf("checks = %+v; want bucket %s, workflow=%v", checks, tc.bucket, tc.workflow)
			}
			scope := scm.NewCheckScope(&fakeRequiredHost{required: []scm.RequiredCheck{{Context: "build"}}}, scm.CheckScopePolicy{}, "main", nil)
			scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks)
			if scoped.Ready() != tc.ready || len(scoped.Relevant) != 1 || scoped.Relevant[0].ExpectedMissing || scoped.Relevant[0].Bucket != tc.bucket {
				t.Fatalf("required build must follow the current workflow observation: %+v; want ready=%v", scoped, tc.ready)
			}
		})
	}
}

// A green rerun whose current job has not reached the rollup yet must not take
// over the stale job's identity: nothing current satisfies the job's
// requirement until the job registers.
func TestGetChecksGreenJoblessRerunDoesNotSpeakForStaleJobs(t *testing.T) {
	t.Parallel()
	job := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"FAILURE","completedAt":"2026-10-14T09:00:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions","databaseId":15368},"workflowRun":{"runAttempt":2,"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"push","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "]")},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
		// Attempt 2 owns a new build job that has not reached the rollup yet.
		attemptJobsCommand(7, 2): attemptJobsResponse(9),
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || !checks[0].WorkflowRun || checks[0].Bucket != scm.CheckBucketPass || len(checks[0].Supersedes) != 0 {
		t.Fatalf("checks = %+v; want only the green workflow run, superseding nothing", checks)
	}
	required := []scm.RequiredCheck{{Context: "build", AppID: 15368}}
	scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: required, Derived: true}}, "main", nil)
	if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); scoped.Ready() {
		t.Fatalf("a green jobless rerun satisfied the build requirement: %+v", scoped)
	}
}

// One workflow run for pull_request and again for workflow_dispatch produces
// two independent same-named jobs; collapsing one into the other would hide
// a failure or manufacture one.
func TestGetChecksCollapseKeepsJobsOfDifferentTriggers(t *testing.T) {
	t.Parallel()
	pass := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	fail := `{"__typename":"CheckRun","databaseId":6,"name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-10-14T11:00:00Z","completedAt":"2026-10-14T11:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/2/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/1"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"workflow_dispatch","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/2"}]}]`
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + pass + "," + fail + "]")},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	buckets := map[scm.CheckBucket]int{}
	for _, c := range checks {
		if c.Name == "build" {
			buckets[c.Bucket]++
		}
	}
	if buckets[scm.CheckBucketPass] != 1 || buckets[scm.CheckBucketFail] != 1 {
		t.Fatalf("checks = %+v; want both triggers' build jobs", checks)
	}
}

func TestGetChecksRetiresHistoricalJoblessWorkflowRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, event, branch, status, conclusion string
		jobs, ready                             bool
	}{
		{"passing jobs", "pull_request", "feature", "completed", "success", true, true},
		{"failing jobs", "pull_request", "feature", "completed", "failure", true, false},
		{"queued without jobs", "pull_request", "feature", "queued", "", false, false},
		{"failed without jobs", "pull_request", "feature", "completed", "failure", false, false},
		// A newer passing attempt whose jobs have not reached the rollup yet
		// retires the older build: build stays missing until its job lands.
		{"green without jobs retires older job evidence", "pull_request", "feature", "completed", "success", false, false},
		{"independent event", "workflow_dispatch", "feature", "completed", "success", true, false},
		{"independent branch", "pull_request", "other", "completed", "success", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				job := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
				if tc.jobs {
					newJob := strings.ReplaceAll(job, "/runs/1/", "/runs/3/")
					newJob = strings.ReplaceAll(newJob, `"databaseId":5`, `"databaseId":6`)
					if tc.conclusion == "failure" {
						newJob = strings.ReplaceAll(newJob, `"SUCCESS"`, `"FAILURE"`)
					}
					job += "," + newJob
				}
				runs := []string{
					`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","html_url":"https://github.com/test/repo/actions/runs/1"}`,
					`{"id":2,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","html_url":"https://github.com/test/repo/actions/runs/2"}`,
					fmt.Sprintf(`{"id":3,"workflow_id":10,"name":"CI","event":%q,"head_branch":%q,"status":%q,"conclusion":%q,"html_url":"https://github.com/test/repo/actions/runs/3"}`, tc.event, tc.branch, tc.status, tc.conclusion),
				}
				if reverse {
					runs[0], runs[2] = runs[2], runs[0]
				}
				host := New(githubTestCmdFactory(map[string]githubTestResponse{
					"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
					githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse("[" + job + "]")},
					"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: `[{"total_count":3,"workflow_runs":[` + strings.Join(runs, ",") + `]}]`},
				}), nil, "", "test/repo")
				checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
				if err != nil {
					t.Fatal(err)
				}
				independent := tc.event != "pull_request" || tc.branch != "feature"
				for _, check := range checks {
					if check.WorkflowRun && check.RunID == 2 && !independent {
						t.Fatalf("obsolete workflow failure survived: %+v", checks)
					}
				}
				scope := scm.NewCheckScope(&fakeRequiredHost{required: []scm.RequiredCheck{{Context: "build"}}}, scm.CheckScopePolicy{}, "main", nil)
				if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); scoped.Ready() != tc.ready {
					t.Fatalf("readiness = %v, want %v: %+v", scoped.Ready(), tc.ready, scoped)
				}
			}
		})
	}
}

// attemptJobsCommand is the read of the jobs a run's current attempt owns.
func attemptJobsCommand(run, attempt int) string {
	return fmt.Sprintf("gh api --method GET repos/test/repo/actions/runs/%d/attempts/%d/jobs -f per_page=100 --paginate --slurp", run, attempt)
}

// attemptJobsResponse lists the job IDs a run's current attempt owns.
func attemptJobsResponse(ids ...int) githubTestResponse {
	jobs := make([]string, len(ids))
	for i, id := range ids {
		jobs[i] = fmt.Sprintf(`{"id":%d}`, id)
	}
	return githubTestResponse{stdout: fmt.Sprintf(`[{"total_count":%d,"jobs":[%s]}]`, len(ids), strings.Join(jobs, ","))}
}

// checksForAttempts collects checks for one head. Runs on a rerun attempt read
// their current attempt's jobs; currentRun7 lists the job IDs run 7's attempt
// 2 owns.
func checksForAttempts(t *testing.T, jobs, runs string, currentRun7 ...int) []scm.Check {
	t.Helper()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr view 5 --repo test/repo --json headRefOid --jq .headRefOid":                                       {stdout: "abc123\n"},
		githubCommitChecksCommand("", "test/repo", "abc123"):                                                     {stdout: githubCommitChecksResponse(jobs)},
		"gh api --method GET repos/test/repo/actions/runs -f head_sha=abc123 -f per_page=100 --paginate --slurp": {stdout: runs},
		attemptJobsCommand(7, 2): attemptJobsResponse(currentRun7...),
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "5", HeadSHA: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	return checks
}

// Runs 7 and 8 pass; run 7 is then rerun and fails before creating jobs. A
// rerun keeps its run ID, so execution order comes from the current attempt's
// start: the rerun is the newest attempt and its failure must stand.
func TestGetChecksRerunOfAnOlderRunIDIsTheNewestAttempt(t *testing.T) {
	t.Parallel()
	job7 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	job8 := `{"__typename":"CheckRun","databaseId":6,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T11:00:00Z","completedAt":"2026-10-14T11:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/8/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T12:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"},` +
		`{"id":8,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
	checks := checksForAttempts(t, "["+job7+","+job8+"]", runs)
	failed := false
	for _, c := range checks {
		if c.Bucket == scm.CheckBucketFail {
			failed = true
		}
		if c.Name == "build" && c.Bucket == scm.CheckBucketPass {
			t.Fatalf("an older attempt's passing build outlived the newest failed attempt: %+v", checks)
		}
	}
	if !failed {
		t.Fatalf("the newest attempt's failure was retired: %+v", checks)
	}
}

// An active rerun of run 7 outranks run 8's earlier success: its pending job
// stands for the current attempt and carries that attempt's start.
func TestGetChecksActiveRerunOfAnOlderRunIDIsNotCollapsedAway(t *testing.T) {
	t.Parallel()
	job7 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	job8 := `{"__typename":"CheckRun","databaseId":6,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T11:00:00Z","completedAt":"2026-10-14T11:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/8/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"in_progress","conclusion":null,"run_started_at":"2026-10-14T12:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"},` +
		`{"id":8,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
	// Attempt 2 owns a new build job that has not reached the rollup yet.
	checks := checksForAttempts(t, "["+job7+","+job8+"]", runs, 9)
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPending || !standsFor(checks[0], "build") {
		t.Fatalf("checks = %+v; want only the active rerun, pending and standing for build", checks)
	}
}

// Run 7 started first but its build job started late; run 8 started later
// and failed before creating jobs. Attempts are ordered by the run's start,
// not the job's, so run 8 supersedes run 7's build.
func TestGetChecksOrdersAttemptsByRunStartNotJobStart(t *testing.T) {
	t.Parallel()
	job7 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T12:00:00Z","completedAt":"2026-10-14T12:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":7,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"},` +
		`{"id":8,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
	checks := checksForAttempts(t, "["+job7+"]", runs)
	if len(checks) != 1 || checks[0].Name != "CI" || checks[0].Bucket != scm.CheckBucketFail || len(checks[0].Supersedes) != 1 || checks[0].Supersedes[0].Name != "build" {
		t.Fatalf("checks = %+v; want run 8's failure superseding run 7's build", checks)
	}
}

// Run 7 passed build, was rerun and failed before jobs, then run 8 also failed
// before jobs. Run 8 retires run 7's result and must carry the build identity
// run 7 took over; otherwise the requirement loses its actionable failure.
func TestGetChecksRetiredJoblessResultPassesItsIdentitiesOn(t *testing.T) {
	t.Parallel()
	job7 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":2,"workflow_runs":[` +
		`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"},` +
		`{"id":8,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T12:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
	checks := checksForAttempts(t, "["+job7+"]", runs)
	if len(checks) != 1 || checks[0].RunID != 8 || checks[0].Bucket != scm.CheckBucketFail || len(checks[0].Supersedes) != 1 || checks[0].Supersedes[0].Name != "build" {
		t.Fatalf("checks = %+v; want run 8's failure carrying build", checks)
	}
	scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{MustPass: []string{"build*"}, Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "lint"}}, Derived: true}}, "main", nil)
	if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, append(checks, scm.Check{Name: "lint", Kind: scm.CheckKindRun, Bucket: scm.CheckBucketPass})); scoped.Ready() {
		t.Fatalf("readiness passed over the failure that owns build: %+v", scoped)
	}
}

// A newer jobless attempt that passed or was skipped has not reported the jobs
// it ran: it must not take over a retired failure's job identities, so the
// required build stays missing until its job registers.
func TestGetChecksGreenJoblessAttemptDoesNotInheritRetiredIdentities(t *testing.T) {
	t.Parallel()
	for _, conclusion := range []string{"success", "skipped"} {
		job7 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
		runs := `[{"total_count":2,"workflow_runs":[` +
			`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"},` +
			`{"id":8,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"` + conclusion + `","run_started_at":"2026-10-14T12:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
		checks := checksForAttempts(t, "["+job7+"]", runs)
		// A passing run 8 retires run 7 without standing for build; a skipped
		// run 8 retires nothing, so run 7's failure keeps standing for it.
		if conclusion == "success" && (len(checks) != 1 || checks[0].RunID != 8 || len(checks[0].Supersedes) != 0) {
			t.Fatalf("%s: checks = %+v; want run 8 alone, standing for no job", conclusion, checks)
		}
		if conclusion == "skipped" && (len(checks) != 1 || checks[0].RunID != 7 || checks[0].Bucket != scm.CheckBucketFail || len(checks[0].Supersedes) != 1) {
			t.Fatalf("%s: checks = %+v; want run 7's failure, still standing for build", conclusion, checks)
		}
		scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "build"}}, Derived: true}}, "main", nil)
		if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); scoped.Ready() {
			t.Fatalf("%s: a green jobless attempt certified the missing build: %+v", conclusion, scoped)
		}
	}
}

// Run 1 passes build, run 2 fails before jobs, run 3 is skipped. A skipped
// attempt cannot retire run 2's failure, which still stands for build.
func TestGetChecksSkippedSuccessorDoesNotRetireAFailure(t *testing.T) {
	t.Parallel()
	job1 := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":3,"workflow_runs":[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/1"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/2"},` +
		`{"id":3,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"skipped","run_started_at":"2026-10-14T12:00:00Z","html_url":"https://github.com/test/repo/actions/runs/3"}]}]`
	checks := checksForAttempts(t, "["+job1+"]", runs)
	scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "build"}}, Derived: true}}, "main", nil)
	if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); scoped.Ready() {
		t.Fatalf("a skipped successor exposed run 1's stale build: %+v", checks)
	}
}

// Every sequence of up to three attempts of one workflow (same event and
// branch, distinct runs started in order) with each outcome must give the same
// readiness for the required job as GitHub's newest meaningful attempt: ready
// exactly when the newest attempt that was not skipped reported a passing job.
// When that attempt failed, a failing check must remain to repair. The job is
// named build, and also CI like its workflow, whose workflow-level results then
// share its name.
func TestGetChecksAttemptSequencesJudgeTheNewestMeaningfulAttempt(t *testing.T) {
	t.Parallel()
	// partial-*: the attempt has reported only a passing lint job so far.
	outcomes := []string{"jobs-pass", "jobs-fail", "jobless-fail", "jobless-pass", "jobless-skip", "pending", "partial-fail", "partial-pass"}
	var sequences [][]string
	var build func(prefix []string)
	build = func(prefix []string) {
		if len(prefix) > 0 {
			sequences = append(sequences, append([]string(nil), prefix...))
		}
		if len(prefix) == 3 {
			return
		}
		for _, o := range outcomes {
			build(append(prefix, o))
		}
	}
	build(nil)
	// The same collection path GetChecks takes after parsing: reconcile jobs
	// with the workflow runs, then collapse same-name reruns.
	h := New(nil, nil, "", "test/repo")
	at := func(hour, minute int) time.Time { return time.Date(2026, 10, 14, hour, minute, 0, 0, time.UTC) }
	job := func(id int64, runID int, name string, bucket scm.CheckBucket, hour int) scm.Check {
		c := scm.Check{Name: name, ProviderID: fmt.Sprintf("github-check-run:%d", id), Kind: scm.CheckKindRun, Workflow: "CI", App: "github-actions",
			Bucket: bucket, StartedAt: at(hour, 0), Link: fmt.Sprintf("https://github.com/test/repo/actions/runs/%d/job/%d", runID, id)}
		if bucket != scm.CheckBucketPending {
			c.CompletedAt = at(hour, 5)
		}
		return c
	}
	for _, jobName := range []string{"build", "CI"} {
		for _, seq := range sequences {
			var jobs, runs []scm.Check
			for i, outcome := range seq {
				id := i + 1
				bucket := scm.CheckBucketPass
				switch outcome {
				case "jobs-fail", "jobless-fail", "partial-fail":
					bucket = scm.CheckBucketFail
				case "jobless-skip":
					bucket = scm.CheckBucketSkip
				case "pending":
					bucket = scm.CheckBucketPending
				}
				runs = append(runs, scm.Check{Name: "CI", ProviderID: fmt.Sprintf("github-workflow-run:%d", id), Kind: scm.CheckKindRun, Bucket: bucket,
					StartedAt: at(10+id, 0), WorkflowID: 10, RunID: int64(id), WorkflowEvent: "pull_request", WorkflowBranch: "feature", WorkflowRun: true,
					Link: fmt.Sprintf("https://github.com/test/repo/actions/runs/%d", id)})
				switch {
				case strings.HasPrefix(outcome, "jobs-"):
					jobs = append(jobs, job(int64(100+id), id, jobName, bucket, 10+id))
				case outcome == "pending":
					jobs = append(jobs, job(int64(100+id), id, jobName, scm.CheckBucketPending, 10+id))
				case strings.HasPrefix(outcome, "partial-"):
					jobs = append(jobs, job(int64(200+id), id, "lint", scm.CheckBucketPass, 10+id))
				}
			}
			newest := ""
			for i := len(seq) - 1; i >= 0; i-- {
				if seq[i] != "jobless-skip" {
					newest = seq[i]
					break
				}
			}
			checks := h.collapseLatestByName(h.appendUnrepresentedWorkflowRuns(jobs, runs, nil))
			scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: jobName}}, Derived: true}}, "main", nil)
			scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks)
			if got, want := scoped.Ready(), newest == "jobs-pass"; got != want {
				t.Errorf("%s %v: ready = %v, want %v; checks = %+v", jobName, seq, got, want, checks)
			}
			// A failure must stay repairable when something ties it to the
			// job: the job was reported at some point, or it shares its
			// workflow's name. A workflow that never reported the job leaves
			// it missing, which still blocks.
			reported := jobName == "CI"
			for _, outcome := range seq {
				if strings.HasPrefix(outcome, "jobs-") || outcome == "pending" {
					reported = true
				}
			}
			if reported && (newest == "jobs-fail" || newest == "jobless-fail" || newest == "partial-fail") {
				repairable := false
				for _, c := range scoped.Relevant {
					if c.Bucket == scm.CheckBucketFail && !c.ExpectedMissing {
						repairable = true
					}
				}
				if !repairable {
					t.Errorf("%s %v: no failing check left to repair; relevant = %+v", jobName, seq, scoped.Relevant)
				}
			}
		}
	}
}

// Attempt 2 of run 7 failed but has so far reported only a passing lint; the
// old build is stale. The failed attempt keeps standing for build until a
// current build replaces it, so readiness never turns green on lint alone.
func TestGetChecksPartiallyReportedFailedRerunStandsForUncoveredJobs(t *testing.T) {
	t.Parallel()
	oldBuild := `{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	lint := `{"__typename":"CheckRun","databaseId":6,"name":"lint","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-14T11:01:00Z","completedAt":"2026-10-14T11:03:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`
	runs := `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`
	checks := checksForAttempts(t, "["+oldBuild+","+lint+"]", runs, 6)
	scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll}, "main", nil)
	scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks)
	if scoped.Ready() {
		t.Fatalf("readiness went green on lint while the failed attempt still stands for build: %+v", checks)
	}
	found := false
	for _, c := range checks {
		if c.WorkflowRun && c.Bucket == scm.CheckBucketFail && len(c.Supersedes) == 1 && c.Supersedes[0].Name == "build" {
			found = true
		}
	}
	if !found {
		t.Fatalf("checks = %+v; want the failed attempt standing for build", checks)
	}
}

// Collapse merges only proven replacements: two same-named runs of one
// workflow whose trigger is unknown on either side stay separate, so a newer
// passing run cannot hide an older failing independent invocation.
func TestCollapseRequiresAKnownMatchingTrigger(t *testing.T) {
	t.Parallel()
	h := New(nil, nil, "", "test/repo")
	older := scm.Check{Name: "build", Kind: scm.CheckKindRun, WorkflowID: 10, Bucket: scm.CheckBucketFail, StartedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), Link: "https://github.com/test/repo/actions/runs/1/job/1"}
	newer := scm.Check{Name: "build", Kind: scm.CheckKindRun, WorkflowID: 10, Bucket: scm.CheckBucketPass, StartedAt: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC), Link: "https://github.com/test/repo/actions/runs/2/job/2"}
	for name, events := range map[string][2]string{"both unknown": {"", ""}, "one unknown": {"pull_request", ""}} {
		a, b := older, newer
		a.WorkflowEvent, b.WorkflowEvent = events[0], events[1]
		if got := h.collapseLatestByName([]scm.Check{a, b}); len(got) != 2 {
			t.Errorf("%s: collapsed to %+v; want both kept", name, got)
		}
	}
	a, b := older, newer
	a.WorkflowEvent, b.WorkflowEvent = "pull_request", "pull_request"
	a.WorkflowBranch, b.WorkflowBranch = "feature", "feature"
	if got := h.collapseLatestByName([]scm.Check{a, b}); len(got) != 1 || got[0].Bucket != scm.CheckBucketPass {
		t.Errorf("a proven rerun was not collapsed to the newest: %+v", got)
	}
}

// standsFor reports whether c is, or stands for, a check named name.
func standsFor(c scm.Check, name string) bool {
	if c.Name == name {
		return true
	}
	for _, identity := range c.Supersedes {
		if identity.Name == name {
			return true
		}
	}
	return false
}

// "Re-run failed jobs" gives every successful job it leaves untouched a new
// job record in the new attempt that keeps the original, earlier times. The
// attempt's own job list, not timing, proves those copies current: a required
// build that passed in attempt 1 still counts once only test was rerun.
func TestGetChecksPartialRerunKeepsUntouchedSuccessfulJobs(t *testing.T) {
	t.Parallel()
	job := func(id int, name, conclusion, start, done string) string {
		return fmt.Sprintf(`{"__typename":"CheckRun","databaseId":%d,"name":%q,"status":"COMPLETED","conclusion":%q,"startedAt":%q,"completedAt":%q,"detailsUrl":"https://github.com/test/repo/actions/runs/7/job/%d","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}`, id, name, conclusion, start, done, id)
	}
	rollup := "[" + strings.Join([]string{
		job(5, "build", "SUCCESS", "2026-10-14T10:00:00Z", "2026-10-14T10:05:00Z"),
		job(6, "test", "FAILURE", "2026-10-14T10:00:00Z", "2026-10-14T10:06:00Z"),
		job(15, "build", "SUCCESS", "2026-10-14T10:00:00Z", "2026-10-14T10:05:00Z"),
		job(16, "test", "SUCCESS", "2026-10-14T11:01:00Z", "2026-10-14T11:06:00Z"),
	}, ",") + "]"
	runs := `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`
	checks := checksForAttempts(t, rollup, runs, 15, 16)
	scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "build"}, {Context: "test"}}, Derived: true}}, "main", nil)
	if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); !scoped.Ready() {
		t.Fatalf("the untouched build of a partial rerun was lost: checks = %+v", checks)
	}
}

func TestGetChecksFirstAttemptPreservesCompletedJobs(t *testing.T) {
	t.Parallel()
	for _, conclusion := range []string{"SUCCESS", "FAILURE"} {
		t.Run(conclusion, func(t *testing.T) {
			jobs := fmt.Sprintf(`[
				{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":%q,"startedAt":"2026-10-14T10:00:00Z","completedAt":"2026-10-14T10:05:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}},
				{"__typename":"CheckRun","databaseId":6,"name":"approval","status":"IN_PROGRESS","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/6","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}
			]`, conclusion)
			runs := `[{"total_count":1,"workflow_runs":[{"id":7,"workflow_id":10,"run_attempt":1,"name":"CI","event":"pull_request","head_branch":"feature","status":"in_progress","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}]}]`
			checks := checksForAttempts(t, jobs, runs)
			wantBucket := scm.CheckBucketPass
			if conclusion == "FAILURE" {
				wantBucket = scm.CheckBucketFail
			}
			found := false
			for _, check := range checks {
				if check.ProviderID == "github-check-run:5" {
					found = true
					if check.Bucket != wantBucket || check.State != conclusion || !check.CompletedAt.Equal(time.Date(2026, 10, 14, 10, 5, 0, 0, time.UTC)) {
						t.Fatalf("completed build lost its verdict: %+v", check)
					}
				}
			}
			if !found {
				t.Fatal("completed build disappeared")
			}
			scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "build"}}, Derived: true}, ExternalChecks: []string{"approval"}}, "main", nil)
			if scoped := scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks); scoped.Ready() != (conclusion == "SUCCESS") {
				t.Fatalf("required build verdict was masked by pending approval: %+v", scoped)
			}
		})
	}
}

// An Actions URL does not make a check an Actions job. Only the publishing
// app can establish ownership, including when the current attempt omits the
// check, a newer run retires the linked run, or an unknown attempt is pending.
func TestGetChecksPreservesActionsLinkedNonActionsChecks(t *testing.T) {
	t.Parallel()
	for _, app := range []string{"security-audit", ""} {
		for _, scenario := range []string{"rerun", "retirement", "unknown-pending-attempt"} {
			t.Run(fmt.Sprintf("app=%q/%s", app, scenario), func(t *testing.T) {
				run7 := `{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}`
				buildRun := 7
				runCount := 1
				var extraRun string
				switch scenario {
				case "retirement":
					run7 = strings.Replace(run7, `"run_attempt":2`, `"run_attempt":1`, 1)
					extraRun = `,{"id":8,"workflow_id":10,"run_attempt":1,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}`
					buildRun = 8
					runCount = 2
				case "unknown-pending-attempt":
					run7 = strings.Replace(run7, `"run_attempt":2`, `"run_attempt":0`, 1)
					run7 = strings.Replace(run7, `"status":"completed","conclusion":"success"`, `"status":"in_progress","conclusion":null`, 1)
				}
				jobs := fmt.Sprintf(`[
					{"__typename":"CheckRun","databaseId":9,"name":"audit-security","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-10-14T11:00:00Z","completedAt":"2026-10-14T12:00:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/7","checkSuite":{"app":{"slug":%q,"databaseId":1234},"workflowRun":{"workflow":{"name":"Audit"}}}},
					{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","completedAt":"2026-10-14T12:00:00Z","detailsUrl":"https://github.com/test/repo/actions/runs/%d/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}
				]`, app, buildRun)
				checks := checksForAttempts(t, jobs, fmt.Sprintf(`[{"total_count":%d,"workflow_runs":[%s%s]}]`, runCount, run7, extraRun), 5)
				want := scm.Check{
					Name: "audit-security", ProviderID: "github-check-run:9", Kind: scm.CheckKindRun,
					App: app, AppID: 1234, Workflow: "Audit", Bucket: scm.CheckBucketFail, State: "FAILURE",
					StartedAt: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
					Link: "https://github.com/test/repo/actions/runs/7", HeadSHA: "abc123",
				}
				var audit *scm.Check
				for i := range checks {
					if checks[i].ProviderID == want.ProviderID {
						audit = &checks[i]
					}
				}
				if audit == nil {
					t.Errorf("independent failing check disappeared: %+v", checks)
				} else if !reflect.DeepEqual(*audit, want) {
					t.Errorf("independent check was altered: got %+v, want %+v", *audit, want)
				}
				for _, policy := range []scm.CheckScopePolicy{
					{RequiredChecks: scm.RequiredChecksAll},
					{RequiredChecks: scm.RequiredChecksAuto, MustPass: []string{"audit-*"}, Resolved: &scm.CheckScopeResolution{Required: []scm.RequiredCheck{{Context: "build"}}, Derived: true}},
				} {
					scoped := scm.NewCheckScope(nil, policy, "main", nil).Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks)
					if scoped.Ready() {
						t.Errorf("%s: independent failing check allowed readiness: %+v", policy.RequiredChecks, scoped)
					}
				}
			})
		}
	}
}

func TestGetChecksPreservesActionsLinkedStatusesDuringReconciliation(t *testing.T) {
	t.Parallel()
	for _, retirement := range []bool{false, true} {
		t.Run(fmt.Sprintf("retirement=%v", retirement), func(t *testing.T) {
			jobs := `[
				{"__typename":"StatusContext","id":"audit","context":"audit-status","state":"FAILURE","targetUrl":"https://github.com/test/repo/actions/runs/7"},
				{"__typename":"CheckRun","databaseId":5,"name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/test/repo/actions/runs/7/job/5","checkSuite":{"app":{"slug":"github-actions"},"workflowRun":{"workflow":{"name":"CI"}}}}
			]`
			run7 := `{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","html_url":"https://github.com/test/repo/actions/runs/7"}`
			runs := `[{"total_count":1,"workflow_runs":[` + run7 + `]}]`
			if retirement {
				run7 = strings.Replace(run7, `"run_attempt":2`, `"run_attempt":1`, 1)
				runs = `[{"total_count":2,"workflow_runs":[` + run7 + `,{"id":8,"workflow_id":10,"run_attempt":1,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T11:00:00Z","html_url":"https://github.com/test/repo/actions/runs/8"}]}]`
			}
			checks := checksForAttempts(t, jobs, runs, 5)
			found := false
			for _, check := range checks {
				if check.ProviderID == "github-status:audit" {
					found = true
					if check.Kind != scm.CheckKindStatus || check.Bucket != scm.CheckBucketFail || check.RunID != 0 || check.WorkflowID != 0 {
						t.Fatalf("independent status was altered: %+v", check)
					}
				}
			}
			if !found {
				t.Fatalf("failing status disappeared: %+v", checks)
			}
			scope := scm.NewCheckScope(nil, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll, MustPass: []string{"audit-*"}}, "main", nil)
			if scope.Apply(context.Background(), &scm.PR{HeadSHA: "abc123"}, checks).Ready() {
				t.Fatal("failing must-pass status allowed readiness")
			}
		})
	}
}
