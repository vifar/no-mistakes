package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRMultilinePayloadByteContract(t *testing.T) {
	const token = `"co-author (quoted): **Codex** <code><ai@example.com></code>"`
	const document = "{\r\n\t\"escaped\\u005fkey\" : [\r\n  {\"description\" : " + token + ", \"number\": 1.2300e+4},\r\n  \"slash\\/unicode\\u0041\", true, null\r\n ],\r\n \"tail\": \"unchanged\"\r\n}  "
	for _, doc := range []string{document, "[\n" + document + ",\n {\"another\": \"unchanged\"}\n]"} {
		for _, surround := range [][2]string{{"", ""}, {"```json\n", "\n```"}, {"Command output:\n", "\nFinished."}} {
			input := surround[0] + doc + surround[1]
			want := surround[0] + strings.Replace(doc, token, `""`, 1) + surround[1]
			if got := prDisplayBody(input); got != want {
				t.Errorf("recorded bytes changed beyond the filtered value:\ngot %q\nwant %q", got, want)
			}
			const human = `"Co-Authored-By: Ada Lovelace <ada@example.com>"`
			const labelled = `"co-author (quoted): Ada Lovelace <ada@example.com>"`
			input = strings.Replace(input, token, human, 1)
			want = strings.Replace(input, human, labelled, 1)
			if got := prDisplayBody(input); got != want {
				t.Errorf("human payload lost its label or unrelated bytes:\ngot %q\nwant %q", got, want)
			}
		}
	}
	configured := strings.Replace(document, token, `"Build-Agent: worker"`, 1)
	wantConfigured := strings.Replace(configured, `"Build-Agent: worker"`, `""`, 1)
	if got := prDisplayBody(configured, "Build-Agent: worker"); got != wantConfigured {
		t.Errorf("configured recorded value survived or changed unrelated bytes: %q", got)
	}
	for _, input := range []string{
		"```json\n[\n {\"order\": 3, \"text\": \"A\\u0042\\/C\"},\n [false, null]\n]\n```",
		"{ incomplete JSON\nordinary prose mentioning co-author (quoted): stays unchanged.\nVerification passed.",
		"[documentation](https://example.com)\nVerification passed.",
	} {
		if got := prDisplayBody(input); got != input {
			t.Errorf("unrelated or incomplete recorded content changed:\ngot %q\nwant %q", got, input)
		}
	}
}

func TestPRMultilinePayloadLabelsAHumanKey(t *testing.T) {
	const tail = "\n \"escape\": \"\\u0041\\/\\\\\",\n \"numbers\": [1.00, -2e-3]\n}"
	input := "{\n \"Co-Authored-By: Ada <ada@example.com>\" : \"unchanged\"," + tail
	want := "{\n \"co-author (quoted): Ada <ada@example.com>\" : \"unchanged\"," + tail
	if got := prDisplayBody(input); got != want {
		t.Errorf("human key must be labelled with every other byte unchanged:\ngot  %q\nwant %q", got, want)
	}
}

func TestPRMultilinePayloadPublicationPaths(t *testing.T) {
	const raw = "{\n  \"items\": [\n    {\"description\": \"co-author (quoted): **Codex** <code><ai@example.com></code>\"},\n    {\"description\": \"Co-Authored-By: Ada Lovelace <ada@example.com>\"}\n  ],\n  \"unchanged\": \"ordinary evidence\"\n}"
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			appendix := compliantPipelineBody(t, testPipelineHeadSHA)
			sctx := &pipeline.StepContext{Config: &config.Config{}}
			paths := map[string]func(*testing.T) string{
				"display": func(t *testing.T) string { return prDisplayText(raw) },
				"notice":  func(t *testing.T) string { return prDisplayNoticeText(raw) },
				"caption": func(t *testing.T) string {
					return prDisplayFindings(types.Findings{Artifacts: []types.TestArtifact{{Label: raw}}}).Artifacts[0].Label
				},
				"content": func(t *testing.T) string {
					return prDisplayFindings(types.Findings{Artifacts: []types.TestArtifact{{Content: raw}}}).Artifacts[0].Content
				},
				"captured": func(t *testing.T) string {
					root := t.TempDir()
					path := filepath.Join(root, "recorded.json")
					if err := os.WriteFile(path, []byte("Captured evidence:\n```json\n"+raw+"\n```"), 0600); err != nil {
						t.Fatal(err)
					}
					text, ok := embeddedArtifactText(types.TestArtifact{Path: path}, testingSummaryOptions{evidenceRoot: root}, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes})
					if !ok {
						t.Fatal("recorded evidence omitted")
					}
					return text
				},
				"narrative": func(t *testing.T) string { return assemblePRBody(sctx, raw, "Low risk", "", appendix, 0, provider) },
				"unowned-restamp": func(t *testing.T) string {
					got, rebound, err := rebindOwnedPRAttestation(appendix+"\n"+raw, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
					if err != nil || !rebound {
						t.Fatalf("restamp: %t, %v", rebound, err)
					}
					return got
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
							parts.before = raw
						case "after":
							parts.after = "\n\n" + raw
						case "appendix":
							block += "\n" + raw
						}
						if restamp {
							got, rebound, err := rebindOwnedPRAttestation(parts.before+"\n\n"+wrapPRAppendix(block)+"\n\n"+parts.after, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
							if err != nil || !rebound {
								t.Fatalf("restamp: %t, %v", rebound, err)
							}
							if _, err := parsePROwnedBody(got); err != nil {
								t.Fatal(err)
							}
							return got
						}
						got, err := composeOwnedPRContent(parts, "fix: preserve evidence", block, 0)
						if err != nil {
							t.Fatal(err)
						}
						return got.Body
					}
				}
			}
			for name, publish := range paths {
				t.Run(name, func(t *testing.T) {
					got := publish(t)
					if strings.Contains(got, "Codex") || strings.Contains(got, "ai@example.com") {
						t.Fatalf("recorded AI attribution survived: %s", got)
					}
					for _, want := range []string{"co-author (quoted): Ada Lovelace", "ada@example.com", "ordinary evidence"} {
						if !strings.Contains(got, want) {
							t.Errorf("publication lost %q: %s", want, got)
						}
					}
				})
			}
		})
	}
}
