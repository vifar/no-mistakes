package steps

import (
	"encoding/json"
	"fmt"
	"html"
	"net/mail"
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
	content = strings.TrimRight(content, " \t\r")
	for len(content) > 0 {
		bullet = bullet || reviewMarkdownList.MatchString(content)
		var unwrapped string
		switch content[0] {
		case '>':
			unwrapped = content[1:]
		case '-', '+':
			if !reviewMarkdownList.MatchString(content) {
				return content, indented && !bullet
			}
			unwrapped = content[1:]
		case '"', '\'', '`', '*':
			n := 1
			for n < len(content) && content[n] == content[0] {
				n++
			}
			if content[0] == '*' && reviewMarkdownList.MatchString(content) {
				unwrapped = content[1:]
			} else if len(content) >= 2*n && strings.HasSuffix(content, content[:n]) {
				unwrapped = content[n : len(content)-n]
			} else {
				return content, indented && !bullet
			}
		default:
			return content, indented && !bullet
		}
		content = strings.TrimLeft(unwrapped, " \t\r")
		indented = indented || content != unwrapped
		content = strings.TrimRight(content, " \t\r")
	}
	return content, indented && !bullet
}

func reviewAttributionStart(line string) bool {
	if !reviewTrailerKey.MatchString(line) {
		return reviewGeneratedWith.MatchString(line)
	}
	key, _, _ := strings.Cut(line, ":")
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "co-authored-by", "assisted-by", "signed-off-by", "reviewed-by", "co-committed-by":
		return true
	}
	return git.IsAIAttributionTrailer(line)
}

func reviewAttributionAddressComplete(line string) bool {
	_, value, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	if i := strings.LastIndex(value, "<"); i >= 0 {
		value = value[i:]
	}
	_, err := mail.ParseAddress(strings.TrimSpace(value))
	return err == nil
}

func reviewConfiguredAttributionMatches(line string, configuredTrailers []string) bool {
	decoded, _ := reviewAttributionLine(html.UnescapeString(line))
	for _, trailer := range configuredTrailers {
		decodedConfigured, _ := reviewAttributionLine(html.UnescapeString(trailer))
		if decoded == decodedConfigured {
			return true
		}
	}
	return false
}

func reviewAttributionContinuation(first, following string) (string, bool) {
	next, continuation := reviewAttributionLine(following)
	if !continuation || next == "" || reviewTrailerKey.MatchString(next) {
		return first, false
	}
	key, value, hasKey := strings.Cut(first, ":")
	addressForm := false
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "co-authored-by", "assisted-by", "signed-off-by", "reviewed-by", "co-committed-by":
		addressForm = hasKey && strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "<>@") && !reviewAttributionAddressComplete(first)
	}
	if strings.EqualFold(strings.TrimSpace(key), "Assisted-by") && strings.Contains(value, ":") {
		addressForm = false
	}
	_, addressErr := mail.ParseAddress(next)
	emptyGenerated := reviewGeneratedWith.MatchString(first) && strings.HasSuffix(strings.ToLower(first), "with")
	joined := first + " " + next
	return joined, (addressForm && addressErr == nil) || (emptyGenerated && git.IsAIAttributionTrailer(joined))
}

func reviewAttributionEntry(lines []string, start int, configuredTrailers []string) (end int, unfolded string, entry, ai bool) {
	end = start + 1
	if reviewConfiguredAttributionMatches(lines[start], configuredTrailers) {
		return end, lines[start], true, true
	}
	firstLine := html.UnescapeString(lines[start])
	unfolded, _ = reviewAttributionLine(firstLine)
	if end < len(lines) {
		nextLine := html.UnescapeString(lines[end])
		paired, _ := reviewAttributionLine(firstLine + "\n" + nextLine)
		first, following, hasPair := strings.Cut(paired, "\n")
		if !reviewAttributionStart(unfolded) && hasPair && first != unfolded && reviewAttributionStart(first) {
			if joined, ok := reviewAttributionContinuation(first, following); ok {
				unfolded = joined
				end++
			}
		}
		if end == start+1 && reviewAttributionStart(unfolded) {
			if joined, ok := reviewAttributionContinuation(unfolded, nextLine); ok {
				unfolded = joined
				end++
			}
		}
	}
	if !reviewAttributionStart(unfolded) {
		return end, unfolded, false, false
	}
	ai = git.IsAIAttributionTrailer(unfolded)
	if end > start+1 {
		rawPair, _ := reviewAttributionLine(strings.Join(lines[start:end], "\n"))
		rawFirst, rawNext, _ := strings.Cut(rawPair, "\n")
		rawFirst, _ = reviewAttributionLine(rawFirst)
		rawNext, _ = reviewAttributionLine(rawNext)
		ai = ai || reviewConfiguredAttributionMatches(rawFirst+" "+rawNext, configuredTrailers)
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
