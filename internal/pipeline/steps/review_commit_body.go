package steps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

var reviewTrailerKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*[ \t]*:`)

const reviewAttributionLeadingWrappers = " \t\r>\"'`*-"

func normalizeReviewAttributionLine(line string) string {
	return strings.TrimRight(strings.TrimLeft(line, reviewAttributionLeadingWrappers), " \t\r\"'`*-")
}

func stripReviewAIAttribution(text string, configuredTrailers []string) string {
	var kept []string
	lines := strings.Split(text, "\n")
	for start := 0; start < len(lines); {
		end := start + 1
		entry := normalizeReviewAttributionLine(lines[start])
		if reviewTrailerKey.MatchString(entry) || git.IsAIAttributionTrailer(entry) {
			for end < len(lines) {
				next := normalizeReviewAttributionLine(lines[end])
				if next == "" || strings.TrimLeft(lines[end], reviewAttributionLeadingWrappers) == lines[end] || reviewTrailerKey.MatchString(next) || git.IsAIAttributionTrailer(next) {
					break
				}
				entry += " " + next
				end++
			}
		}
		attribution := git.IsAIAttributionTrailer(entry)
		for _, trailer := range configuredTrailers {
			if strings.EqualFold(entry, normalizeReviewAttributionLine(trailer)) {
				attribution = true
				break
			}
		}
		if !attribution {
			kept = append(kept, lines[start:end]...)
		}
		start = end
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// This is also the FixSummary returned to the executor for step_rounds. The
// record is inserted after Execute returns, so reading a prior DB row here
// would attach the wrong round's rationale to the commit.
func reviewFixCommitSummary(summary string, producer *agent.Result, configuredTrailers []string) (subject, body string) {
	raw := summary
	reason := "fixer returned no summary"
	if producer != nil && producer.Output != nil {
		var report commitSummary
		if err := json.Unmarshal(producer.Output, &report); err != nil {
			raw = ""
			reason = "fixer returned no readable structured summary"
		} else {
			raw = report.Summary
		}
	}
	body = stripReviewAIAttribution(raw, configuredTrailers)
	if body == "" {
		if strings.TrimSpace(raw) != "" {
			reason = "summary contained only attribution"
		}
		return "", "body: unavailable - " + reason
	}
	// Keep the existing subject's normalization and punctuation behavior.
	subject = strings.Trim(strings.Join(strings.Fields(body), " "), " \t\r\n\"'.;:,-")
	if body == NoChangesAppliedSummary {
		body = "Fixer summary: " + body
	}
	return subject, body
}

func reviewFixCommitBody(summary, previousFindings string, configuredTrailers []string) string {
	var b strings.Builder
	b.WriteString(summary)
	var findings Findings
	if err := json.Unmarshal([]byte(previousFindings), &findings); err != nil {
		b.WriteString("\n\nRound findings unavailable (payload could not be read).")
		return b.String()
	}
	if len(findings.Items) == 0 {
		return b.String()
	}
	b.WriteString("\n\nRound findings:")
	const maxFindings = 5
	for i, finding := range findings.Items {
		if i == maxFindings {
			fmt.Fprintf(&b, "\n- %d more findings omitted", len(findings.Items)-i)
			break
		}
		text := stripReviewAIAttribution(finding.Description, configuredTrailers)
		if file := stripReviewAIAttribution(finding.File, configuredTrailers); file != "" {
			text = file + ": " + text
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			text = "finding description unavailable"
		}
		if runes := []rune(text); len(runes) > 240 {
			text = string(runes[:237]) + "..."
		}
		b.WriteString("\n- " + text)
	}
	return b.String()
}
