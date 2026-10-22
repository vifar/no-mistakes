package steps

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The pull request body is generated, but the evidence it embeds is not: a
// test that exercises the attribution feature quotes the trailer it created.
// Every consumer that scans the body for attribution - the merge bar, a
// reviewer - then reads that quoted trailer as an attribution of THIS work,
// and refuses a pull request whose author wrote none.
//
// The line is broken, not dropped: the evidence still shows what the test did.
func TestNeutralizeAIAttributionBreaksTheLinesAReaderTakesForAttribution(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"co-authored trailer", "Co-Authored-By: Codex <bot@example.invalid>"},
		{"co-authored trailer with tag-like mailbox", "Co-Authored-By: Codex <code@example.com>"},
		{"co-authored trailer with a name", "Co-Authored-By: Claude <noreply@anthropic.com>"},
		{"generated with", "Generated with [Claude Code](https://claude.ai/code)"},
		{"generated with plain", "Generated with Claude Code"},
		{"generated with codex", "Generated with Codex"},
		{"generated with inline code", "Generated with `Codex`"},
		{"generated with double inline code", "Generated with ``Codex``"},
		{"co-authored inline code", "Co-Authored-By: `Codex` <bot@example.invalid>"},
		{"co-authored double inline code", "Co-Authored-By: ``Codex`` <code@example.com>"},
		{"model trailer", "Co-Authored-By: gpt-5.5 <model@example.com>"},
		{"assisted trailer", "Assisted-by: OpenCode"},
		{"claude session", "Claude-Session: session-id"},
		{"codex session", "Codex-Session: session-id"},
		{"robot emoji", "\U0001F916 Generated with Codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, line := range []string{
				tc.line,
				html.EscapeString(tc.line),
				"`" + tc.line + "`",
				"`` " + html.EscapeString(tc.line) + " ``",
				"**" + tc.line + "**",
				"<code>" + tc.line + "</code>",
				"**`" + tc.line + "`**",
				"* " + tc.line,
				"- " + tc.line,
				"1. " + tc.line,
				"> " + tc.line,
				"> > " + tc.line,
				"<code>" + tc.line + "</code>",
				"  - " + tc.line,
				"Generated with **" + "Codex**",
				"Co-Authored-By: **" + "Codex** <bot@example.invalid>",
			} {
				body := "## Testing\n\n```text\n" + line + "\n```\n"
				got := neutralizeAttestationMarkers(scm.ProviderGitHub, body)
				want := "## Testing\n\n```text\n<no-mistakes: AI attribution line neutralized>\n```\n"
				if got != want {
					t.Fatalf("published text = %q, want %q", got, want)
				}
			}
		})
	}
}

// A trailer that names a PERSON is legitimate attribution and stays.
func TestNeutralizeAIAttributionKeepsAHumanTrailer(t *testing.T) {
	for _, line := range []string{
		"Co-Authored-By: A Person <person@example.com>",
		"Co-Authored-By: Claude Smith <human@example.com>",
		"Co-Authored-By: `Claude Smith` <human@example.com>",
		"Co-Authored-By: ``Claude Smith`` <human@example.com>",
		"Co-Authored-By: Claude Smith <code@example.com>",
		"Co-Authored-By: Claude <human@example.com>",
		"Assisted-by: Claude Smith <human@example.com>",
		"We tested fixtures generated with Codex.",
	} {
		for _, text := range []string{line, html.EscapeString(line), "`" + line + "`", "`` " + html.EscapeString(line) + " ``", "<code>" + line + "</code>"} {
			if got := neutralizeAttestationMarkers(scm.ProviderGitHub, text); got != text {
				t.Fatalf("human attribution or prose was changed: %q", got)
			}
		}
	}
}

// The neutralization applies to the whole body, so the evidence section is
// covered as well as the prose.
func TestNeutralizeAIAttributionAppliesInsideEmbeddedEvidence(t *testing.T) {
	body := "## Testing\n\n```text\nCo-Authored-By: Codex <bot@example.invalid>\n```\n"
	got := neutralizeAIAttribution(body)
	if strings.Contains(got, "Codex") {
		t.Fatalf("the embedded evidence still names an AI: %q", got)
	}
	if !strings.Contains(got, "## Testing") {
		t.Fatalf("the body lost its structure: %q", got)
	}
}

func TestPRSummaryFiltersAttributionBeforeRendering(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "evidence.txt"), []byte("captured output"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, source := range []string{"tested", "tested multiline", "tested code span", "summary", "artifact text", "artifact description", "artifact label", "artifact file", "scenario", "finding", "risk", "error"} {
			for _, human := range []bool{false, true} {
				for _, wrapper := range []string{"", "`", "``"} {
					t.Run(string(provider)+"/"+source+"/"+wrapper+"/"+map[bool]string{false: "AI", true: "human"}[human], func(t *testing.T) {
						line := "Co-Authored-By: Codex <code@example.com>"
						if human {
							line = "Co-Authored-By: Claude Smith <human@example.com>"
						}
						line = wrapper + line + wrapper
						findings := types.Findings{}
						switch source {
						case "tested":
							findings.Tested = []string{line}
						case "tested multiline":
							findings.Tested = []string{"Recorded check\n" + line}
						case "tested code span":
							findings.Tested = []string{"`" + line + "`"}
						case "summary":
							findings.TestingSummary = "Recorded check\n" + line
						case "artifact text":
							findings.Artifacts = []types.TestArtifact{{Kind: "text", Label: "Evidence", Content: "Recorded check\n" + line}}
						case "artifact description":
							findings.Artifacts = []types.TestArtifact{{Kind: "text", Label: "Evidence", Path: filepath.Join(root, "evidence.txt"), Content: line}}
						case "artifact label":
							findings.Artifacts = []types.TestArtifact{{Kind: "text", Label: "Recorded check\n" + line, Content: "captured output"}}
						case "artifact file":
							if err := os.WriteFile(filepath.Join(root, "attribution.txt"), []byte(line), 0o644); err != nil {
								t.Fatal(err)
							}
							findings.Artifacts = []types.TestArtifact{{Kind: "text", Label: "Evidence", Path: filepath.Join(root, "attribution.txt")}}
						case "scenario":
							findings.Scenarios = []types.TestScenario{{Name: "Recorded check", Result: types.ScenarioResultPass, Live: true, Evidence: "Recorded check\n" + line}}
						case "finding":
							findings.Items = []types.Finding{{Severity: types.FindingSeverityWarning, Description: line}}
						case "risk":
							findings.RiskLevel = "high"
							findings.RiskRationale = line
						}
						encoded, err := json.Marshal(findings)
						if err != nil {
							t.Fatal(err)
						}
						raw := string(encoded)
						steps := []*db.StepResult{{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw}}
						rounds := map[string][]*db.StepRound{"test": {{Round: 1, Trigger: "initial", FindingsJSON: &raw}}}
						if source == "risk" {
							steps[0].StepName = types.StepReview
						} else if source == "error" {
							steps[0].Status = types.StepStatusFailed
							steps[0].Error = &line
							rounds = nil
						}
						var outputs []string
						if source != "finding" && source != "risk" && source != "error" {
							outputs = append(outputs, buildTestingSummary(steps, rounds, testingSummaryOptions{flavor: prBodyFlavorFor(provider), includeTestedDetails: true, repoRoot: root, evidenceRoot: root}))
							if !strings.HasPrefix(source, "tested") {
								outputs = append(outputs, BuildTestingSummaryForPRWithProvider(steps, rounds, "https://github.com/example/repo", "head", root, root, nil, provider))
							}
						}
						if strings.HasPrefix(source, "tested") || source == "scenario" || source == "finding" || source == "risk" || source == "error" {
							pipeline, risk := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, provider)
							if source == "risk" {
								outputs = append(outputs, risk)
							} else {
								outputs = append(outputs, pipeline)
							}
						}
						for _, output := range outputs {
							if human {
								if !strings.Contains(html.UnescapeString(output), "Co-Authored-By: Claude Smith <human@example.com>") {
									t.Fatalf("human attribution lost from published summary:\n%s", output)
								}
							} else if strings.Contains(output, "Codex") || !strings.Contains(output, "AI attribution line neutralized") {
								t.Fatalf("AI attribution was not neutralized in published summary:\n%s", output)
							}
						}
					})
				}
			}
		}
	}
}
