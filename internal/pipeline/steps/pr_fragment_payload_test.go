package steps

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The intent: a published body carries no AI-named co-author in any form, cut
// payloads included. A human co-author is the labelled field and every other
// byte (line endings, escapes, cut markers, prose) is unchanged.
func TestPRFragmentPayloadByteContract(t *testing.T) {
	const ai = `"<code>co-author (quoted): <code>Codex</code> <ai@example.com></code>"`
	const human = `"Co-Authored-By: Ada Lovelace <ada@example.com>"`
	const labelled = `"co-author (quoted): Ada Lovelace \u003cada@example.com\u003e"`
	for _, input := range []string{
		"{\r\n\t\"description\" : " + ai + ",\r\n \"human\": " + human + ",\r\n \"escape\": \"A\\u0042\\/C\",\r\n",
		"\"description\" : " + ai + ",\n\"human\": " + human,
		"{\n \"cutoff\": \"unfinished\n\n... [20000 bytes truncated] ...\n\n \"description\" : " + ai + ",\n \"human\": " + human + "\n}",
		"```json\n{\n \"description\" : " + ai + ", \"human\": " + human + ",\n```\nVerification passed.",
	} {
		want := strings.Replace(strings.Replace(input, ai, `""`, 1), human, labelled, 1)
		if got := prDisplayBody(input); got != want {
			t.Errorf("cut payload: AI must be absent and the rest unchanged:\ngot  %q\nwant %q", got, want)
		}
	}
	for _, input := range []string{
		`{"broken": "invalid\q\"ordinary", "escape": "A\u0042\/C",`,
		"{\n \"ordinary\": \"A\\u0042\\/C\",\n \"cutoff\": \"unfinished",
		`Ordinary prose mentions "co-author (quoted): Codex <ai@example.com>" as a field.`,
		`"Ordinary prose mentioning co-author (quoted): Codex stays verbatim."`,
	} {
		if got := prDisplayBody(input); got != input {
			t.Errorf("invalid payload or ordinary prose changed:\ngot %q\nwant %q", got, input)
		}
	}
}

func TestPRFragmentPayloadPublicationPaths(t *testing.T) {
	const raw = "{\n  \"description\": \"<code>co-author (quoted): <code>Codex</code> <ai@example.com></code>\",\n  \"human\": \"Co-Authored-By: Ada Lovelace <ada@example.com>\",\n  \"unchanged\": \"ordinary evidence\\u0021\",\n... [20000 bytes truncated] ...\n}"
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
						t.Errorf("publication carried an AI co-author: %s", got)
					}
					for _, want := range []string{"Ada Lovelace", "ordinary evidence", "bytes truncated"} {
						if !strings.Contains(got, want) {
							t.Errorf("publication lost %q: %s", want, got)
						}
					}
				})
			}
		})
	}
}

// The intent: the AI co-author is absent from captured evidence cut at the
// size limit; the human co-author is the labelled field.
func TestPRFragmentLargeCapturedPayload(t *testing.T) {
	const ai = `"<code>co-author (quoted): <code>Codex</code> <ai@example.com></code>"`
	const human = `"Co-Authored-By: Ada Lovelace <ada@example.com>"`
	capture := "{\n \"description\": " + ai + ",\n \"human\": " + human + ",\n \"escape\": \"A\\u0042\\/C\",\n \"large\": \"" + strings.Repeat("x", 24*1024) + "\",\n \"tail-ai\": " + ai + ",\n \"tail-human\": " + human + ",\n \"tail\": \"tail-evidence\"\n}"
	root := t.TempDir()
	path := filepath.Join(root, "capture.json")
	if err := os.WriteFile(path, []byte(capture), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(types.Findings{Artifacts: []types.TestArtifact{{Label: "Large capture", Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(payload)
	steps := []*db.StepResult{{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw}}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, compact := range []bool{false, true} {
			opts := testingSummaryOptions{flavor: prBodyFlavorFor(provider), repoRoot: root, evidenceRoot: root, compactArtifacts: compact}
			body := html.UnescapeString(buildTestingSummary(steps, nil, opts))
			for _, want := range []string{`co-author (quoted): Ada Lovelace`, `A\u0042\/C`, "bytes truncated", "tail-evidence"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s compact=%t lost %q: %s", provider, compact, want, body)
				}
			}
			for _, banned := range []string{"Codex", "ai@example.com", "Co-Authored-By"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s compact=%t published %q: %s", provider, compact, banned, body)
				}
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != capture {
		t.Fatalf("publication changed captured evidence: %v", err)
	}
}
