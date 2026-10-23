package steps

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const fallbackAgentFooter = "🤖 Generated with [Claude Code](https://claude.ai/code)"
const fallbackAgentTitle = "fix(pipeline): preserve repair rationale"
const fallbackAgentNarrative = "Preserve the current repair rationale."

func fallbackAgentRecordedFooter(t *testing.T, footer string) string {
	t.Helper()
	payload, err := json.MarshalIndent(struct {
		Description string `json:"description"`
		Status      string `json:"status"`
	}{Description: footer, Status: "captured fallback response"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return "Captured fallback response:\n\n```json\n" + string(payload) + "\n```"
}

func assertFallbackAgentFooterAbsent(t *testing.T, output string) {
	t.Helper()
	for _, forbidden := range []string{"Claude Code", "claude.ai/code", "🤖"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("fallback agent attribution reached published output: %q", output)
		}
	}
}

func TestPRFallbackAgentFooterPublicationPaths(t *testing.T) {
	t.Parallel()
	for _, footer := range []string{
		fallbackAgentFooter,
		"**" + fallbackAgentFooter + "**",
		html.EscapeString("<code>" + fallbackAgentFooter + "</code>"),
	} {
		t.Run(footer, func(t *testing.T) {
			fixture := fallbackAgentNarrative + "\n\n" + footer + "\n\n" + fallbackAgentRecordedFooter(t, footer) + "\n\n" + humanCoauthorDisclosure
			appendix := compliantPipelineBody(t, testPipelineHeadSHA)
			checkBody := func(t *testing.T, body string) {
				t.Helper()
				assertFallbackAgentFooterAbsent(t, body)
				for _, retained := range []string{fallbackAgentNarrative, humanCoauthorDisclosure, `"status": "captured fallback response"`} {
					if !strings.Contains(body, retained) {
						t.Fatalf("publication lost ordinary text %q: %q", retained, body)
					}
				}
			}
			for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps, scm.ProviderForgejo, scm.ProviderGitea, scm.ProviderOrigin, scm.ProviderUnknown} {
				for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
					t.Run(string(provider)+"/"+mode, func(t *testing.T) {
						sctx := &pipeline.StepContext{Config: &config.Config{}}
						sctx.Config.PR.Appendix = mode
						checkBody(t, assembleDraftPRBody(sctx, fixture, "Low risk.", "", appendix, 0, provider))
						checkBody(t, assemblePRBody(sctx, fixture, "Low risk.", "", appendix, 0, provider))
						checkBody(t, buildPRBody(fixture, "Low risk.", "", appendix, sctx, provider))
					})
				}
			}
			for _, component := range []string{"before", "after", "appendix"} {
				t.Run("owned/"+component, func(t *testing.T) {
					parts, evidence := prOwnedBody{}, appendix
					switch component {
					case "before":
						parts.before = fixture
					case "after":
						parts.after = "\n\n" + fixture
					case "appendix":
						evidence += "\n\n" + fixture
					}
					content, err := composeOwnedPRContent(parts, fallbackAgentTitle+"\n\n"+footer, evidence, 0)
					if err != nil {
						t.Fatal(err)
					}
					checkBody(t, content.Body)
					if strings.TrimSpace(content.Title) != fallbackAgentTitle {
						t.Fatalf("owned title = %q, want %q", content.Title, fallbackAgentTitle)
					}
					body := parts.before + "\n\n" + wrapPRAppendix(evidence) + "\n\n" + parts.after
					updated, rebound, err := rebindOwnedPRAttestation(body, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
					if err != nil || !rebound {
						t.Fatalf("owned restamp: rebound=%t, err=%v", rebound, err)
					}
					checkBody(t, updated)
					if _, err := parsePROwnedBody(updated); err != nil {
						t.Fatalf("filtered restamp invalidated appendix integrity: %v", err)
					}
				})
			}
			t.Run("unowned-restamp", func(t *testing.T) {
				updated, rebound, err := rebindOwnedPRAttestation(fixture+"\n\n"+appendix, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
				if err != nil || !rebound {
					t.Fatalf("legacy restamp: rebound=%t, err=%v", rebound, err)
				}
				checkBody(t, updated)
			})
			t.Run("renderPRTitle", func(t *testing.T) {
				for _, format := range []string{"", "{{.Title}}"} {
					sctx := &pipeline.StepContext{Config: &config.Config{}, Run: &db.Run{Branch: "feature"}}
					sctx.Config.PR.TitleFormat = format
					for _, raw := range []string{fallbackAgentTitle, fallbackAgentTitle + "\n\n" + footer} {
						got, err := renderPRTitle(sctx, raw)
						if err != nil || got != fallbackAgentTitle {
							t.Fatalf("rendered title = %q, %v, want %q", got, err, fallbackAgentTitle)
						}
					}
				}
			})
		})
	}
}

func TestPRFallbackAgentFooterDraftPublication(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps, scm.ProviderForgejo, scm.ProviderGitea, scm.ProviderOrigin, scm.ProviderUnknown} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			recorded := fallbackAgentRecordedFooter(t, fallbackAgentFooter)
			ag := &mockAgent{name: "fallback", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, err := json.Marshal(prContent{Title: fallbackAgentTitle + "\n\n" + fallbackAgentFooter, Body: "## What Changed\n\n" + fallbackAgentNarrative + "\n\n" + fallbackAgentFooter + "\n\n" + recorded + "\n\n" + humanCoauthorDisclosure})
				return &agent.Result{Output: payload}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertFallbackAgentFooterAbsent(t, content.Title+"\n"+content.Body)
			if content.Title != fallbackAgentTitle || !strings.Contains(content.Body, fallbackAgentNarrative) || !strings.Contains(content.Body, humanCoauthorDisclosure) || !strings.Contains(content.Body, `"status": "captured fallback response"`) {
				t.Fatalf("draft lost ordinary title or narrative: %+v", content)
			}
			sctx.Config.PR.TitleFormat = "{{.Title}}"
			title, err := (&PRStep{}).draftConfiguredPRTitle(sctx, "feature", "main", base)
			if err != nil || title != fallbackAgentTitle {
				t.Fatalf("configured draft title = %q, %v", title, err)
			}
		})
	}
	t.Run("template", func(t *testing.T) {
		sctx, ag, _ := templateTestContext(t)
		recorded := fallbackAgentRecordedFooter(t, fallbackAgentFooter)
		ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{Title: fallbackAgentTitle + "\n\n" + fallbackAgentFooter, Body: filledPRTemplate + "\n" + fallbackAgentFooter + "\n\n" + recorded + "\n\n" + humanCoauthorDisclosure})
			return &agent.Result{Output: payload}, err
		}
		content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertFallbackAgentFooterAbsent(t, content.Title+"\n"+content.Body)
		if content.Title != fallbackAgentTitle || !strings.Contains(content.Body, "Add a Bar helper.") || !strings.Contains(content.Body, humanCoauthorDisclosure) || !strings.Contains(content.Body, `"status": "captured fallback response"`) {
			t.Fatalf("template draft lost ordinary title or narrative: %+v", content)
		}
	})
}
