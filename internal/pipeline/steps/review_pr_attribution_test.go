package steps

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRReviewConfiguredTrailersAreRemovedFromRecordedFindingDisplays(t *testing.T) {
	t.Parallel()
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const human = "Co-Authored-By: Claude Smith <claude@example.com>"
	const prose = "The Build-Agent: private-identifier value is mentioned in ordinary prose."
	trailers := []string{
		"Build-Agent: private-identifier",
		"Co-Authored-By: private:writer <opaque@example.com>",
		"Build-Identity: {{.Agent}} / {{.Model}}",
	}
	for _, snapshots := range []bool{false, true} {
		for _, field := range []string{"File", "Description"} {
			for _, location := range []string{"historical", "current"} {
				for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
					t.Run(fmt.Sprintf("snapshots=%t/%s/%s/%s", snapshots, field, location, provider), func(t *testing.T) {
						dir, base, head := setupGitRepo(t)
						sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
						if !snapshots {
							sctx.Config.Commit.Trailers = trailers
						}
						finding := Finding{ID: "r1", Severity: "warning", File: "feature.go", Description: "Guard unsafe value"}
						text := "\n> Build-Agent: private-identifier\nCo-Authored-By: private:writer <opaque@example.com>\nCo-Authored-By: private:writer\n <opaque@example.com>\nBuild-Identity: acp:private:writer / opaque-model\n\n" + disclosure + "\n\n" + human + "\n" + prose + "\nBuild-Agent: unconfigured-human"
						if field == "File" {
							finding.File += text
						} else {
							finding.Description += text
						}
						payload, err := json.Marshal(Findings{Items: []Finding{finding}})
						if err != nil {
							t.Fatal(err)
						}
						initial, final := string(payload), `{"findings":[]}`
						if location == "current" {
							initial, final = final, initial
						}
						step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
						if err != nil {
							t.Fatal(err)
						}
						if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
							t.Fatal(err)
						}
						if err := sctx.DB.SetStepFindings(step.ID, final); err != nil {
							t.Fatal(err)
						}
						global, repo := []byte(nil), []byte(nil)
						if snapshots {
							global = []byte("{}\n")
							repo = []byte("commit:\n  trailers:\n    - 'Build-Agent: private-identifier'\n    - 'Co-Authored-By: private:writer <opaque@example.com>'\n    - 'Build-Identity: {{.Agent}} / {{.Model}}'\n")
						}
						summary := "Guard unsafe value"
						for i, recorded := range []string{initial, final} {
							trigger := "initial"
							if i == 1 {
								trigger = "auto_fix"
							}
							if _, err := sctx.DB.InsertReviewStepRoundWithProvenance(step.ID, i+1, trigger, &recorded, &summary, head, base, base, global, repo, 1); err != nil {
								t.Fatal(err)
							}
							if _, err := sctx.DB.InsertAgentInvocation(db.AgentInvocation{RunID: sctx.Run.ID, StepName: "review", Round: i + 1, Purpose: "review", Agent: "acp:private:writer", Model: "opaque-model", SessionMode: db.InvocationModeCold, ExitStatus: "ok"}); err != nil {
								t.Fatal(err)
							}
						}
						markdown, _, _ := (&PRStep{}).buildPipelineSection(sctx, provider)
						for _, forbidden := range []string{"&gt; Build-Agent: private-identifier", "> Build-Agent: private-identifier", "Co-Authored-By: private:writer <opaque@example.com>", "Build-Identity: acp:private:writer / opaque-model", "opaque@example.com"} {
							if strings.Contains(markdown, forbidden) {
								t.Errorf("configured trailer reached published finding: %q", forbidden)
							}
						}
						if strings.Count(markdown, "Build-Agent: private-identifier") != 1 {
							t.Error("exact configured trailer must survive only inside ordinary prose")
						}
						for _, kept := range []string{disclosure, "co-author (quoted): Claude Smith", prose, "Build-Agent: unconfigured-human"} {
							if !strings.Contains(html.UnescapeString(markdown), kept) {
								t.Errorf("ordinary finding content lost: %q in %q", kept, markdown)
							}
						}
						if field == "Description" && !strings.Contains(markdown, disclosure+"\n\n"+"co-author (quoted): Claude Smith") {
							t.Error("finding paragraph spacing lost")
						}
						rounds, err := sctx.DB.GetRoundsByStep(step.ID)
						if err != nil {
							t.Fatal(err)
						}
						steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if len(rounds) != 2 || rounds[0].FindingsJSON == nil || rounds[1].FindingsJSON == nil || *rounds[0].FindingsJSON != initial || *rounds[1].FindingsJSON != final {
							t.Fatal("PR rendering changed raw recorded findings")
						}
						found := false
						for _, stored := range steps {
							if stored.ID == step.ID {
								found = true
								if stored.FindingsJSON == nil || *stored.FindingsJSON != final {
									t.Fatal("PR rendering changed raw step findings")
								}
							}
						}
						if !found {
							t.Fatal("review step disappeared")
						}
					})
				}
			}
		}
	}
}

// Exercise the public renderer with persisted findings. A clean repair summary
// must not let historical findings reintroduce attribution into the PR.
func TestBuildPipelineSummary_ReviewAttributionDoesNotLeakFromRoundFindings(t *testing.T) {
	t.Parallel()
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const human = "Co-Authored-By: Claude <claude@example.com>"
	const recorder = "Generated with the repository recorder"
	const foldedHuman = "> Co-Authored-By: Claude\n> <claude@example.com>"
	input, err := json.Marshal(Findings{Items: []Finding{{
		ID: "r1", Severity: "warning",
		File:        "feature.go\nCodex-Session: disposable\n> Co-Authored-By: Claude\n> <noreply@anthropic.com>\n**Co-Authored-By: Codex <ai@example.com>**",
		Description: "Guard unsafe value\n> Co-Authored-By: Claude\n> <noreply@anthropic.com>\n**Co-Authored-By: Codex <ai@example.com>**\nCo-Authored-By: gpt-5.5 <model@example.com>\n> Generated with Codex\nAssisted-by: acp:cursor:model\n🤖 Generated with [Claude Code](https://claude.ai/code)\n" + recorder + "\n\n" + disclosure + "\n" + human + "\n" + foldedHuman,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	initial, clean, summary := string(input), `{"findings":[]}`, "Guard unsafe value"
	for _, unresolved := range []bool{false, true} {
		final := clean
		if unresolved {
			final = initial
		}
		step := &db.StepResult{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final}
		rounds := map[string][]*db.StepRound{"review": {
			{Round: 1, Trigger: "initial", FindingsJSON: &initial},
			{Round: 2, Trigger: "auto_fix", FindingsJSON: &final, FixSummary: &summary},
		}}
		for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
			t.Run(string(provider), func(t *testing.T) {
				markdown, _ := BuildPipelineSummaryFor([]*db.StepResult{step}, rounds, testPipelineHeadSHA, provider)
				for _, forbidden := range []string{"Co-Authored-By: gpt-5.5", "Codex-Session: disposable", "Generated with Codex", "Assisted-by: acp:cursor:model", "🤖 Generated with [Claude Code]", "ai@example.com"} {
					if strings.Contains(markdown, forbidden) {
						t.Errorf("Review findings reintroduced attribution %q:\n%s", forbidden, markdown)
					}
				}
				if !strings.Contains(html.UnescapeString(markdown), strings.Replace(foldedHuman, "Co-Authored-By:", "co-author (quoted):", 1)) || !strings.Contains(markdown, disclosure) || !strings.Contains(markdown, "co-author (quoted): Claude") || !strings.Contains(markdown, recorder+"\n\n"+disclosure) {
					t.Errorf("disclosure or human co-author lost:\n%s", markdown)
				}
				if strings.Contains(html.UnescapeString(markdown), "noreply@anthropic.com") {
					t.Errorf("folded AI pair published:\n%s", markdown)
				}
				if *step.FindingsJSON != final || *rounds["review"][0].FindingsJSON != initial {
					t.Fatal("rendering changed recorded findings")
				}
			})
		}
	}
}
