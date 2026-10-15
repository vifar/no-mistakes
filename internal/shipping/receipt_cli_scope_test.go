//go:build e2e && !windows

package shipping

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Exercise the public receipt command in a separate product process. Git and
// durable run state are real and disposable; only the external GitHub CLI is
// simulated. The oracle is the required-check precedence and recorded-policy
// contract, not the implementation's classification of its own input.
func TestReceiptCLIRecordedCheckScope(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "no-mistakes")
	provider := filepath.Join(root, "fakecli")
	for _, target := range []struct{ output, pkg string }{
		{cli, "github.com/kunchenguid/no-mistakes/cmd/no-mistakes"},
		{provider, "github.com/kunchenguid/no-mistakes/internal/pipeline/fakecli"},
	} {
		build := exec.Command("go", "build", "-o", target.output, target.pkg)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", target.pkg, err, out)
		}
	}
	// fakecli's canned PR-facts endpoint names test/repo. Supply this test's
	// independent exact PR identity, forwarding all other reads to fakecli.
	wrapper := "#!/bin/sh\nfor arg do\ncase \"$arg\" in repos/owner/project/pulls/7) printf '%s\\n' \"$FAKE_CLI_PR_FACTS_JSON\"; exit 0;; esac\ndone\nexec '" + strings.ReplaceAll(provider, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "gh"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	const buildPass = `{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions","appId":15368}`
	const approveFail = `{"name":"approve","state":"FAILURE","bucket":"fail"}`
	const botFail = `{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}`
	const botPending = `{"name":"Greptile Review","state":"PENDING","bucket":"pending","app":"greptile-apps"}`
	const comment = `[{"author":"greptile-apps[bot]","path":"source.txt","line":1,"body":"Unresolved concern"}]`
	// The newer workflow has no jobs, so the product must retain both its
	// own identity and the older job's identity when deciding readiness.
	const ciRerun = `[{"id":1,"workflow_id":10,"name":"CI","status":"completed","conclusion":"success","html_url":"https://github.com/owner/project/actions/runs/1"},{"id":2,"workflow_id":10,"name":"CI","status":"completed","conclusion":"failure","html_url":"https://github.com/owner/project/actions/runs/2"}]`
	const approvalRerun = `[{"id":1,"workflow_id":20,"name":"Final Approval","status":"completed","conclusion":"success","html_url":"https://github.com/owner/project/actions/runs/1"},{"id":2,"workflow_id":20,"name":"Final Approval","status":"completed","conclusion":"failure","html_url":"https://github.com/owner/project/actions/runs/2"}]`
	const oldBuild = `{"name":"build","workflow":"CI","state":"SUCCESS","bucket":"pass","app":"github-actions","appId":15368,"link":"https://github.com/owner/project/actions/runs/1/job/5"}`
	const oldUnit = `{"name":"unit","workflow":"CI","state":"SUCCESS","bucket":"pass","app":"github-actions","appId":15368,"link":"https://github.com/owner/project/actions/runs/1/job/6"}`
	const oldApproval = `{"name":"approve","workflow":"Final Approval","state":"SUCCESS","bucket":"pass","app":"github-actions","link":"https://github.com/owner/project/actions/runs/1/job/7"}`
	for _, tc := range []struct {
		name, checks, protection, rules, comments, refusal string
		runs                                             string
		policy                                             scm.CheckScopePolicy
	}{
		{name: "auto ignores outside approval failure", checks: buildPass + "," + approveFail},
		{name: "required failure refuses", checks: `{"name":"build","state":"FAILURE","bucket":"fail"}`, refusal: "all live checks green"},
		{name: "external outranks required and must_pass", checks: buildPass + "," + approveFail, protection: `{"contexts":["build","approve"]}`, policy: scm.CheckScopePolicy{MustPass: []string{"approve"}, ExternalChecks: []string{"approve"}}},
		{name: "all keeps outside failure actionable", checks: buildPass + "," + approveFail, policy: scm.CheckScopePolicy{RequiredChecks: "all"}, refusal: "all live checks green"},
		{name: "all still respects external exclusions", checks: buildPass + "," + approveFail, policy: scm.CheckScopePolicy{RequiredChecks: "all", ExternalChecks: []string{"approve"}}},
		{name: "must_pass rejects wrong workflow", checks: buildPass + `,{"name":"lint","workflow":"Other","state":"SUCCESS","bucket":"pass"}`, policy: scm.CheckScopePolicy{MustPass: []string{"Required / lint"}}, refusal: "all live checks green"},
		{name: "must_pass accepts correct workflow", checks: buildPass + `,{"name":"lint","workflow":"Required","state":"SUCCESS","bucket":"pass"}`, policy: scm.CheckScopePolicy{MustPass: []string{"Required / lint"}}},
		{name: "missing required check refuses", checks: buildPass, protection: `{"contexts":["build","deploy"]}`, refusal: "all live checks green"},
		{name: "pending required check refuses", checks: buildPass + `,{"name":"deploy","state":"PENDING","bucket":"pending"}`, protection: `{"contexts":["build","deploy"]}`, refusal: "all live checks green"},
		{name: "wrong producing app refuses", checks: buildPass, protection: `{"checks":[{"context":"build","app_id":99}]}`, refusal: "all live checks green"},
		{name: "forbidden protection falls back to all", checks: buildPass + "," + approveFail, protection: "403", refusal: "all live checks green"},
		{name: "ruleset adds a missing requirement", checks: buildPass, rules: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ruleset-check"}]}}]`, refusal: "all live checks green"},
		{name: "pending informational bot refuses", checks: buildPass + "," + botPending, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "all live checks green"},
		{name: "pending external bot does not block", checks: buildPass + "," + botPending, policy: scm.CheckScopePolicy{ReviewBotComments: "always", ExternalChecks: []string{"Greptile Review"}}},
		{name: "actions job named Greptile is not a bot", checks: buildPass + `,{"name":"Greptile Review","state":"PENDING","bucket":"pending","app":"github-actions"}`, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}},
		{name: "failed informational bot unresolved comments refuse", checks: buildPass + "," + botFail, comments: comment, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "no unresolved review bot comments"},
		{name: "failed informational bot without comments permits", checks: buildPass + "," + botFail, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}},
		{name: "passing informational bot unresolved comments refuse", checks: buildPass + `,{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps"}`, comments: comment, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "no unresolved review bot comments"},
		{name: "absent bot unresolved comments refuse", checks: buildPass, comments: comment, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "no unresolved review bot comments"},
		{name: "cancelled informational bot comments refuse", checks: buildPass + `,{"name":"Greptile Review","state":"CANCELLED","bucket":"cancel","app":"greptile-apps"}`, comments: comment, policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "no unresolved review bot comments"},
		{name: "unreadable comments refuse", checks: buildPass + "," + botFail, comments: "invalid JSON", policy: scm.CheckScopePolicy{ReviewBotComments: "always"}, refusal: "could not verify review bot comments"},
		{name: "explicit external bot comments are excluded", checks: buildPass + "," + botFail, comments: comment, policy: scm.CheckScopePolicy{ReviewBotComments: "always", ExternalChecks: []string{"Greptile Review"}}},
		{name: "jobless approval rerun retains qualified exclusion", checks: buildPass + "," + oldApproval, runs: approvalRerun, protection: `{"contexts":["build","approve"]}`, policy: scm.CheckScopePolicy{ExternalChecks: []string{"Final Approval / approve*"}}},
		{name: "all superseding identities external permit readiness", checks: buildPass + "," + oldApproval, runs: approvalRerun, protection: `{"contexts":["build","approve"]}`, policy: scm.CheckScopePolicy{ExternalChecks: []string{"Final Approval", "Final Approval / approve*"}}},
		{name: "excluded workflow still blocks as required build", checks: oldBuild, runs: ciRerun, policy: scm.CheckScopePolicy{ExternalChecks: []string{"CI"}}, refusal: "all live checks green"},
		{name: "jobless workflow retains own wildcard must pass", checks: buildPass + "," + oldUnit, runs: ciRerun, policy: scm.CheckScopePolicy{MustPass: []string{"CI*"}}, refusal: "all live checks green"},
		{name: "jobless workflow retains own literal must pass", checks: buildPass + "," + oldUnit, runs: ciRerun, policy: scm.CheckScopePolicy{MustPass: []string{"CI"}}, refusal: "all live checks green"},
		{name: "superseded required build retains app binding", checks: oldBuild, runs: ciRerun, protection: `{"checks":[{"context":"build","app_id":99}]}`, refusal: "all live checks green"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			recordScope(t, f, tc.policy)
			facts := map[string]any{"number": 7, "html_url": f.forge.facts.URL, "state": "open", "merged": false,
				"head": map[string]any{"ref": "feature", "sha": f.run.HeadSHA, "repo": map[string]string{"full_name": "owner/project"}},
				"base": map[string]any{"ref": "main", "sha": f.run.BaseSHA, "repo": map[string]string{"full_name": "owner/project"}}}
			rawFacts, err := json.Marshal(facts)
			if err != nil {
				t.Fatal(err)
			}
			protection, rules := tc.protection, tc.rules
			if protection == "" {
				protection = `{"contexts":["build"]}`
			}
			if rules == "" {
				rules = "[]"
			}
			t.Logf("recorded scope=%+v; checks=[%s]; protection=%s; rules=%s; workflow runs=%s", tc.policy, tc.checks, protection, rules, tc.runs)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cli, "axi", "review-receipt", "--run", f.run.ID, "--phase", "ci-ready")
			cmd.Dir = f.dir
			cmd.Env = append(os.Environ(), "NM_HOME="+filepath.Dir(f.dir), "HOME="+root, "XDG_CONFIG_HOME="+root,
				"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "GIT_CONFIG_GLOBAL="+filepath.Join(root, "no-global-config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0",
				"NO_MISTAKES_TELEMETRY=off", "NO_MISTAKES_NO_UPDATE_CHECK=1", "FAKE_CLI_MODE=ci-gh", "FAKE_CLI_PR_HEAD_SHA="+f.run.HeadSHA,
				"FAKE_CLI_PR_FACTS_JSON="+string(rawFacts), "FAKE_CLI_CHECKS=["+tc.checks+"]", "FAKE_CLI_PROTECTION_JSON="+protection, "FAKE_CLI_RULES_JSON="+rules, "FAKE_CLI_REVIEW_COMMENTS="+tc.comments, "FAKE_CLI_WORKFLOW_RUNS="+tc.runs)
			out, runErr := cmd.CombinedOutput()
			t.Logf("no-mistakes axi review-receipt --phase ci-ready\n%s", out)
			if tc.refusal != "" {
				if runErr == nil || !strings.Contains(string(out), tc.refusal) {
					t.Fatalf("want refusal %q; error=%v output=%s", tc.refusal, runErr, out)
				}
				return
			}
			var receipt Payload
			if runErr != nil || json.Unmarshal(out, &receipt) != nil || receipt.Phase != "ci-ready" || receipt.LocalHeadSHA != f.run.HeadSHA {
				t.Fatalf("want exact-head ci-ready receipt; error=%v output=%s", runErr, out)
			}
		})
	}
}
