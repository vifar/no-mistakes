//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// The publication contract requires exact configured attribution removal before
// provider-specific closing-reference escaping. Exercise the real CLI, daemon,
// configuration loader and provider adapter; only agent and forge I/O are faked.
func TestPRConfiguredAttributionOrderingLiveJourney(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			const attribution = "Build-Agent: worker (Fixes #12)"
			const prose = "Keep the [release notes](https://example.com/notes) unchanged. See #8."
			const recorded = `{"summary":"Build-Agent: worker (Fixes #12)\nRecorded prose stays.","escape":"A\u0042\/C"}`
			data, err := yaml.Marshal(map[string]any{"actions": []any{
				map[string]any{
					"match": "Draft a pull request title and summary for the full branch delta.",
					"structured": map[string]any{
						"title": "fix: filter attribution before closing rewrites",
						"body":  "## What Changed\n\n" + prose + "\n\n" + attribution + "\n\n" + recorded + "\n\nFixes #7",
					},
				},
				map[string]any{"structured": map[string]any{
					"findings": []any{}, "summary": "no issues found", "risk_level": "low",
					"risk_rationale": "disposable publication fixture", "risk_scope": "source-or-external",
					"verdict": "go", "tested": []string{"external response fixture"},
					"testing_summary": "Fixture response for the enclosing real publication journey.",
					"scenarios": []any{map[string]any{
						"name": "external response fixture", "result": "untested", "live": false,
						"evidence": "disposable fixture", "reason": "The enclosing journey tests publication, not this agent response.",
					}}, "artifacts": []any{},
				}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			scenario := filepath.Join(t.TempDir(), "attribution-order.yaml")
			if err := os.WriteFile(scenario, data, 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{
				Agent: "claude", Scenario: scenario,
				GlobalConfigExtra: "commit:\n  trailers:\n    - '" + attribution + "'\n",
			})
			remote := "https://github.com/example/no-mistakes.git"
			if provider == "gitlab" {
				remote = "https://gitlab.com/test/repo.git"
			}
			configureGitURLRewrite(t, h, remote, h.UpstreamDir)
			if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
				t.Fatalf("set isolated origin: %v\n%s", err, out)
			}
			forgeLog := filepath.Join(filepath.Dir(h.AgentLog), "publication.log")
			mrState := filepath.Join(filepath.Dir(h.AgentLog), "mr.json")
			if provider == "github" {
				t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
				t.Setenv("FAKEAGENT_GH_LOG", forgeLog)
				t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")
			} else {
				// The ordinary fake glab persists created bodies. Add the API
				// identity reads the real pre-rebase and publication guards need.
				shim := "#!/bin/sh\n" + `if [ "$1" = api ]; then
  case "$*" in
    *projects/test%2Frepo/merge_requests/99*) cat "$FAKE_CLI_MR_STATE_FILE"; exit ;;
    *projects/test%2Frepo/merge_requests*) printf '[]\n'; exit ;;
    *projects/1*) printf '{"id":1,"path_with_namespace":"test/repo"}\n'; exit ;;
  esac
fi
exec ` + shellQuote(h.FakeCLI) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(h.BinDir, "glab"), []byte(shim), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("FAKE_CLI_MODE", "glab")
				t.Setenv("FAKE_CLI_MR_STATE_FILE", mrState)
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("isolated init: %v\n%s", err, out)
			} else {
				t.Logf("no-mistakes init:\n%s", out)
			}
			const branch = "feature/attribution-order"
			head := h.CommitChange(branch, "feature.txt", "safe\n", "add safe feature")
			if provider == "gitlab" {
				// Preserve enough forge identity for the post-publication readback.
				state, err := json.Marshal(map[string]any{
					"target_branch": "main", "source_branch": branch, "sha": head,
					"source_project_id": 1, "target_project_id": 1,
					"state": "opened", "detailed_merge_status": "mergeable", "head_pipeline": nil,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mrState, state, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", branch); err != nil {
				t.Fatalf("isolated gate push: %v\n%s", err, out)
			} else {
				t.Logf("git push no-mistakes %s:\n%s", branch, out)
			}
			// This journey owns publication only. Harness cleanup stops the daemon
			// after that step, without requiring a simulated remote CI verdict.
			run := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 90*time.Second)
			var body string
			if provider == "github" {
				body = createdPRBody(t, readGHStubInvocations(t, forgeLog))
			} else {
				data, err := os.ReadFile(mrState)
				if err != nil {
					t.Fatal(err)
				}
				var published struct {
					Description string `json:"description"`
				}
				if err := json.Unmarshal(data, &published); err != nil {
					t.Fatal(err)
				}
				body = published.Description
			}
			if strings.Contains(body, "Build-Agent") || strings.Contains(body, "worker") || strings.Contains(body, "#12") {
				t.Fatalf("configured attribution survived publication:\n%s", body)
			}
			for _, want := range []string{prose, "Recorded prose stays.", `"escape":"A\u0042\/C"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("publication lost %q:\n%s", want, body)
				}
			}
			closing := "Fixes `#7`"
			if provider == "gitlab" {
				closing = "Fixes&#32;#7"
			}
			if !strings.Contains(body, closing) {
				t.Fatalf("publication lost closing-reference neutralization %q:\n%s", closing, body)
			}
			t.Logf("Real CLI run %s completed PR publication on %s; body received by disposable forge:\n%s", run.ID, provider, body)
		})
	}
}
