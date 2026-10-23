package steps

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRLabelledMarkupPublicationPaths(t *testing.T) {
	const prose = "Ordinary prose mentioning co-author (quoted): **Codex** stays verbatim."
	const human = "co-author (quoted): **Claude Smith** <human@example.com>"
	type fixtureCase struct{ ai, human string }
	var cases []fixtureCase
	for _, wrapper := range []string{"", "`", "``", "**", "__", "*", "_", "~~", "<code>"} {
		close := wrapper
		if wrapper == "<code>" {
			close = "</code>"
		}
		for _, folded := range []bool{false, true} {
			separator := " "
			if folded {
				separator = "\n "
			}
			for _, escaped := range []bool{false, true} {
				ai := wrapper + "co-author (quoted): Codex" + separator + "<ai@example.com>" + close
				human := wrapper + "co-author (quoted): Claude Smith" + separator + "<human@example.com>" + close
				if escaped {
					ai, human = html.EscapeString(ai), html.EscapeString(human)
				}
				cases = append(cases, fixtureCase{ai, human})
			}
		}
	}
	for _, separator := range []string{" ", "\n "} {
		for _, escaped := range []bool{false, true} {
			ai := "co-author (quoted): **Codex**" + separator + "<ai@example.com>"
			if escaped {
				ai = html.EscapeString(ai)
			}
			cases = append(cases, fixtureCase{ai, human})
		}
	}
	for _, fixture := range cases {
		ai, human := fixture.ai, fixture.human
		t.Run(ai, func(t *testing.T) {
			fixture := ai + "\n\n" + human + "\n\n" + prose + "\n" + humanCoauthorDisclosure
			appendix := compliantPipelineBody(t, testPipelineHeadSHA)
			sctx := &pipeline.StepContext{Config: &config.Config{}}
			check := func(t *testing.T, output string) {
				t.Helper()
				if strings.Contains(output, "ai@example.com") || strings.Contains(strings.ReplaceAll(output, prose, ""), "Codex") {
					t.Errorf("published AI identity survived: %s", output)
				}
				for _, want := range []string{human, prose, humanCoauthorDisclosure} {
					if !strings.Contains(output, want) {
						t.Errorf("publication lost %q: %s", want, output)
					}
				}
			}
			paths := map[string]func(*testing.T) string{
				"text": func(t *testing.T) string { return prDisplayText(fixture) },
				"recorded": func(t *testing.T) string {
					raw, err := json.Marshal(map[string]string{"description": fixture})
					if err != nil {
						t.Fatal(err)
					}
					var decoded map[string]string
					if err := json.Unmarshal([]byte(prDisplayBody(string(raw))), &decoded); err != nil {
						t.Fatal(err)
					}
					return decoded["description"]
				},
				"captured": func(t *testing.T) string {
					root := t.TempDir()
					path := filepath.Join(root, "evidence.txt")
					if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
						t.Fatal(err)
					}
					output, ok := embeddedArtifactText(types.TestArtifact{Path: path}, testingSummaryOptions{evidenceRoot: root}, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes})
					if !ok {
						t.Fatal("captured evidence omitted")
					}
					return output
				},
				"narrative": func(t *testing.T) string {
					return assemblePRBody(sctx, fixture, "Low risk", "", appendix, 0, scm.ProviderGitHub)
				},
			}
			for _, component := range []string{"before", "after", "appendix"} {
				for _, restamp := range []bool{false, true} {
					name := component
					if restamp {
						name += "-restamp"
					}
					paths[name] = func(t *testing.T) string {
						parts, block := prOwnedBody{}, appendix
						switch component {
						case "before":
							parts.before = fixture
						case "after":
							parts.after = fixture
						case "appendix":
							block += "\n" + fixture
						}
						if restamp {
							body := parts.before + "\n\n" + wrapPRAppendix(block) + "\n\n" + parts.after
							output, rebound, err := rebindOwnedPRAttestation(body, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
							if err != nil || !rebound {
								t.Fatalf("restamp: %t, %v", rebound, err)
							}
							if _, err := parsePROwnedBody(output); err != nil {
								t.Fatal(err)
							}
							return output
						}
						content, err := composeOwnedPRContent(parts, "fix: repair", block, 0)
						if err != nil {
							t.Fatal(err)
						}
						return content.Body
					}
				}
			}
			paths["unowned-restamp"] = func(t *testing.T) string {
				output, rebound, err := rebindOwnedPRAttestation(appendix+"\n"+fixture, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
				if err != nil || !rebound {
					t.Fatalf("restamp: %t, %v", rebound, err)
				}
				return output
			}
			for name, render := range paths {
				t.Run(name, func(t *testing.T) { check(t, render(t)) })
			}
		})
	}
}
