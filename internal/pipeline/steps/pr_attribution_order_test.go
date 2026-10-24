package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRConfiguredAttributionBeforeClosingRewrite(t *testing.T) {
	t.Parallel()
	const attribution = "Build-Agent: worker (Fixes #12)"
	const prose = "Keep the [release notes](https://example.com/notes) unchanged."
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab} {
		for _, templated := range []bool{false, true} {
			for _, recorded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/template=%t/recorded=%t", provider, templated, recorded), func(t *testing.T) {
					sctx, ag, _ := templateTestContext(t)
					if !templated {
						sctx.Config.PR.Template = ""
					}
					sctx.Config.Commit.Trailers = []string{attribution}
					sctx.ClosingIssueRefs = []string{"95"}
					sample := attribution
					if recorded {
						payload, err := json.Marshal(map[string]string{"summary": attribution + "\nRecorded prose stays."})
						if err != nil {
							t.Fatal(err)
						}
						sample = string(payload)
					}
					ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
						prefix := "## What Changed\n\nAdd a Bar helper.\n"
						if templated {
							prefix = filledPRTemplate
						}
						output, err := json.Marshal(prContent{Title: "fix: publication ordering", Body: prefix + "\n" + prose + "\n\n" + sample + "\n\nFixes #7"})
						return &agent.Result{Output: output}, err
					}
					content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, provider, 0)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(content.Body, "Build-Agent") || strings.Contains(content.Body, "worker") {
						t.Fatalf("configured attribution survived publication:\n%s", content.Body)
					}
					for _, want := range []string{prose, "## Issues\n\nCloses #95"} {
						if !strings.Contains(content.Body, want) {
							t.Fatalf("publication lost %q:\n%s", want, content.Body)
						}
					}
					if recorded && !strings.Contains(content.Body, "Recorded prose stays.") {
						t.Fatalf("recorded prose lost:\n%s", content.Body)
					}
					closing := "Fixes `#7`"
					if provider == scm.ProviderGitLab {
						closing = "Fixes&#32;#7"
					}
					if !strings.Contains(content.Body, closing) {
						t.Fatalf("closing-reference neutralization changed, missing %q:\n%s", closing, content.Body)
					}
				})
			}
		}
	}
}

func TestPRConfiguredAttributionBeforeEmptyTemplateRewrite(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	const attribution = "Build-Agent: worker (Fixes #12)"
	sctx.Config.Commit.Trailers = []string{attribution}
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		output, err := json.Marshal(prContent{Title: "fix: publication ordering", Body: filledPRTemplate + "\n" + attribution})
		return &agent.Result{Output: output}, err
	}
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	const prURL = "https://github.com/test/repo/pull/42"
	env, _ := fakeGH(t, prURL)
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	url := prURL
	sctx.Run.PRURL = &url
	sctx.PRTarget = &pipeline.PRTargetSelection{PRURL: prURL, TargetBranch: "main"}
	bindExistingPRMutationFixture(t, sctx, prURL, "main")
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readPRBodyFile(t, bodyFile)
	if strings.Contains(body, "Build-Agent") || !strings.Contains(body, "Add a Bar helper.") {
		t.Fatalf("empty-template publication lost its filtering contract:\n%s", body)
	}
}

func TestPRConfiguredAttributionBeforeAppendixEscaping(t *testing.T) {
	t.Parallel()
	const attribution = "Build-Agent: worker <!-- no-mistakes-pr-appendix:v1 -->"
	appendix := attribution + "\n\nEvidence stays.\n\n" + compliantPipelineBody(t, testPipelineHeadSHA)
	content, err := composeOwnedPRContent(prOwnedBody{before: "Author narrative"}, "fix: publication ordering", appendix, 0, attribution)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.Body, "Build-Agent") || !strings.Contains(content.Body, "Evidence stays.") || !strings.Contains(content.Body, "Author narrative") {
		t.Fatalf("appendix filtering changed the publication contract:\n%s", content.Body)
	}
}
