package steps

import (
	"html"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func prDisplayText(text string, configuredTrailers ...string) string {
	lines := strings.Split(text, "\n")
	decoded := make([]string, len(lines))
	for i, line := range lines {
		decoded[i] = html.UnescapeString(line)
	}
	var kept []string
	for start := 0; start < len(lines); {
		end, unfolded, entry, ai := reviewAttributionEntry(decoded, start, configuredTrailers)
		if !ai {
			key, _, _ := strings.Cut(unfolded, ":")
			if entry && strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") && reviewAttributionAddressComplete(unfolded) {
				line := lines[start]
				label := strings.Index(strings.ToLower(line), "co-authored-by")
				if label >= 0 {
					if literalKey := reviewTrailerKey.FindString(line[label:]); literalKey != "" {
						lines[start] = line[:label] + "preserved human co-author:" + line[label+len(literalKey):]
					}
				}
			}
			kept = append(kept, lines[start:end]...)
		} else {
			kept = append(kept, "")
		}
		start = end
	}
	return strings.Join(kept, "\n")
}

func prDisplayFindings(raw types.Findings, configuredTrailers ...string) types.Findings {
	clean := func(text string) string { return prDisplayText(text, configuredTrailers...) }
	raw.Summary = clean(raw.Summary)
	raw.TestingSummary = clean(raw.TestingSummary)
	raw.RiskRationale = clean(raw.RiskRationale)
	raw.Items = append([]types.Finding(nil), raw.Items...)
	for i := range raw.Items {
		raw.Items[i].File = clean(raw.Items[i].File)
		raw.Items[i].Description = clean(raw.Items[i].Description)
	}
	raw.Tested = append([]string(nil), raw.Tested...)
	for i := range raw.Tested {
		raw.Tested[i] = clean(raw.Tested[i])
	}
	raw.Artifacts = append([]types.TestArtifact(nil), raw.Artifacts...)
	for i := range raw.Artifacts {
		raw.Artifacts[i].Label = clean(raw.Artifacts[i].Label)
		raw.Artifacts[i].Content = clean(raw.Artifacts[i].Content)
	}
	raw.Scenarios = append([]types.TestScenario(nil), raw.Scenarios...)
	for i := range raw.Scenarios {
		raw.Scenarios[i].Name = clean(raw.Scenarios[i].Name)
		raw.Scenarios[i].Evidence = clean(raw.Scenarios[i].Evidence)
		raw.Scenarios[i].Reason = clean(raw.Scenarios[i].Reason)
	}
	return raw
}

func parsePRDisplayFindings(text string, configuredTrailers ...string) (types.Findings, error) {
	raw, err := types.ParseFindingsJSON(text)
	if err != nil {
		return raw, err
	}
	return prDisplayFindings(raw, configuredTrailers...), nil
}
