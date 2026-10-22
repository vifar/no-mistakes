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
var reviewMarkdownList = regexp.MustCompile(`^(?:[-*+][ \t]+|[0-9]+[.)][ \t]+)`)
var reviewGeneratedWith = regexp.MustCompile(`(?i)^(?:🤖[ \t]*)?generated[ \t]+with(?:[ \t]|$)`)

func reviewAttributionLine(line string) (string, bool) {
	content := strings.TrimLeft(line, " \t\r")
	indented, bullet := content != line, false
	for {
		bullet = bullet || reviewMarkdownList.MatchString(content)
		unwrapped := strings.TrimLeft(content, ">\"'`*-")
		if unwrapped == content {
			break
		}
		content = strings.TrimLeft(unwrapped, " \t\r")
		indented = indented || content != unwrapped
	}
	return strings.TrimRight(content, " \t\r\"'`*-"), indented && !bullet
}

func reviewAttributionStart(line string, configuredTrailers []string) bool {
	if !reviewTrailerKey.MatchString(line) {
		return reviewGeneratedWith.MatchString(line)
	}
	key, _, _ := strings.Cut(line, ":")
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "co-authored-by", "assisted-by", "signed-off-by", "reviewed-by", "co-committed-by":
		return true
	}
	if git.IsAIAttributionTrailer(line) {
		return true
	}
	for _, trailer := range configuredTrailers {
		configured, _ := reviewAttributionLine(trailer)
		configuredKey, _, ok := strings.Cut(configured, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(configuredKey)) {
			return true
		}
	}
	return false
}

func reviewAttributionEntry(lines []string, start int, configuredTrailers []string) (end int, unfolded string, entry, ai bool) {
	end = start + 1
	unfolded, _ = reviewAttributionLine(lines[start])
	if !reviewAttributionStart(unfolded, configuredTrailers) {
		return end, unfolded, false, false
	}
	for end < len(lines) {
		next, continuation := reviewAttributionLine(lines[end])
		if next == "" || !continuation || reviewAttributionStart(next, configuredTrailers) {
			break
		}
		unfolded += " " + next
		end++
	}
	ai = git.IsAIAttributionTrailer(unfolded)
	for _, trailer := range configuredTrailers {
		configured, _ := reviewAttributionLine(trailer)
		ai = ai || strings.EqualFold(unfolded, configured)
	}
	_, value, hasKey := strings.Cut(unfolded, ":")
	entry = ai || (hasKey && strings.TrimSpace(value) != "" && !reviewGeneratedWith.MatchString(unfolded))
	return end, unfolded, entry, ai
}

func stripReviewAIAttribution(text string, configuredTrailers []string) string {
	var kept []string
	lines := strings.Split(text, "\n")
	for start := 0; start < len(lines); {
		end, _, _, ai := reviewAttributionEntry(lines, start, configuredTrailers)
		if !ai {
			kept = append(kept, lines[start:end]...)
		}
		start = end
	}
	cleaned := strings.Join(kept, "\n")
	if strings.TrimSpace(cleaned) == "" {
		return ""
	}
	return strings.Trim(cleaned, "\r\n")
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
