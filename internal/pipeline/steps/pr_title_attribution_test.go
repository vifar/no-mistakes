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

func TestPRTitleAttributionBoundaries(t *testing.T) {
	const normal = "fix(pipeline): preserve repair summaries"
	for _, footer := range []string{
		"",
		"🤖 Generated with Claude Code",
		"**🤖 Generated with Claude Code**",
		"<code>🤖 Generated with Claude Code</code>",
		"co-author (quoted): **Codex** <code><ai@example.com></code>",
		"**Co-Authored-By: Codex <ai@example.com>**.",
		"**`Co-Authored-By: Codex <ai@example.com>`.**",
		html.EscapeString("~~!<code>?co-author (quoted): Codex <ai@example.com>.</code>;~~?"),
		"`co-author (quoted): Codex <ai@example.com>`.!",
		html.EscapeString("co-author (quoted): Codex `<ai@example.com>`"),
		"Build-Agent: worker",
	} {
		t.Run(footer, func(t *testing.T) {
			raw := normal
			if footer != "" {
				raw += "\n\n" + footer
			}
			for _, format := range []string{"", "{{.Title}}"} {
				sctx := &pipeline.StepContext{Config: &config.Config{}, Run: &db.Run{Branch: "feature"}}
				sctx.Config.Commit.Trailers = []string{"Build-Agent: worker"}
				sctx.Config.PR.TitleFormat = format
				got, err := renderPRTitle(sctx, raw)
				if err != nil || got != normal {
					t.Fatalf("rendered title = %q, %v, want %q", got, err, normal)
				}
			}
			got := redactPRContent(prContent{Title: raw}).Title
			if footer != "Build-Agent: worker" && strings.TrimSpace(got) != normal {
				t.Fatalf("published title = %q", got)
			}
			if footer == "" && got != normal {
				t.Fatalf("ordinary title changed: %q", got)
			}
			owned, err := composeOwnedPRContent(prOwnedBody{}, raw, compliantPipelineBody(t, testPipelineHeadSHA), 0, "Build-Agent: worker")
			if err != nil || strings.TrimSpace(owned.Title) != normal {
				t.Fatalf("owned title = %q, %v", owned.Title, err)
			}
		})
	}
	if got, err := renderPRTitle(nil, "🤖 Generated with Claude Code"); err == nil || got != "" {
		t.Fatalf("attribution-only title accepted: %q, %v", got, err)
	}
}

func TestPRTitleAttributionDraftPaths(t *testing.T) {
	const normal = "fix(pipeline): preserve repair summaries"
	const raw = normal + "\n\n🤖 Generated with Claude Code"
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps, scm.ProviderForgejo, scm.ProviderGitea, scm.ProviderOrigin, scm.ProviderUnknown} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, err := json.Marshal(prContent{Title: raw, Body: "## What Changed\n\nPreserve repair summaries."})
				return &agent.Result{Output: payload}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, provider, 0)
			if err != nil || content.Title != normal {
				t.Fatalf("draft title = %q, %v", content.Title, err)
			}
			sctx.Config.PR.TitleFormat = "{{.Title}}"
			got, err := (&PRStep{}).draftConfiguredPRTitle(sctx, "feature", "main", base)
			if err != nil || got != normal {
				t.Fatalf("configured title = %q, %v", got, err)
			}
			fallback, err := fallbackPRContent(sctx, "M\tfile.go", "", "", compliantPipelineBody(t, head), 0, provider)
			if err != nil || fallback.Title != "update pull request" {
				t.Fatalf("fallback title = %q, %v", fallback.Title, err)
			}
		})
	}
	t.Run("template", func(t *testing.T) {
		sctx, ag, _ := templateTestContext(t)
		ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
			payload, err := json.Marshal(prContent{Title: raw, Body: filledPRTemplate})
			return &agent.Result{Output: payload}, err
		}
		content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
		if err != nil || content.Title != normal {
			t.Fatalf("template title = %q, %v", content.Title, err)
		}
	})
}
