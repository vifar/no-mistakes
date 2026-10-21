package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the public renderer with persisted findings. A clean repair summary
// must not let historical findings reintroduce attribution into the PR.
func TestBuildPipelineSummary_ReviewAttributionDoesNotLeakFromRoundFindings(t *testing.T) {
	t.Parallel()
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const human = "Co-Authored-By: Claude Smith <claude@example.com>"
	input, err := json.Marshal(Findings{Items: []Finding{{
		ID: "r1", Severity: "warning",
		File:        "feature.go\nCodex-Session: disposable",
		Description: "Guard unsafe value\nCo-Authored-By: gpt-5.5 <model@example.com>\n> Generated with Codex\n" + disclosure + "\n" + human,
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
				for _, forbidden := range []string{"Co-Authored-By: gpt-5.5", "Codex-Session: disposable", "Generated with Codex"} {
					if strings.Contains(markdown, forbidden) {
						t.Errorf("Review findings reintroduced attribution %q:\n%s", forbidden, markdown)
					}
				}
				if !strings.Contains(markdown, disclosure) || !strings.Contains(markdown, "Co-Authored-By: Claude Smith") {
					t.Errorf("disclosure or human co-author lost:\n%s", markdown)
				}
				if *step.FindingsJSON != final || *rounds["review"][0].FindingsJSON != initial {
					t.Fatal("rendering changed recorded findings")
				}
			})
		}
	}
}
