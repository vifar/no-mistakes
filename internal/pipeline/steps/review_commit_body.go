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
		if marker := reviewMarkdownList.FindString(content); marker != "" {
			bullet = true
			content = strings.TrimLeft(content[len(marker):], " \t\r")
			continue
		}
		var unwrapped string
		switch content[0] {
		case '>':
			unwrapped = content[1:]
		case '<':
			if !strings.HasPrefix(content, "<code>") || !strings.HasSuffix(content, "</code>") {
				return content, indented && !bullet
			}
			unwrapped = content[len("<code>") : len(content)-len("</code>")]
		case '"', '\'', '`', '*', '_', '~':
			n := 1
			for n < len(content) && content[n] == content[0] {
				n++
			}
			if len(content) >= 2*n && strings.HasSuffix(content, content[:n]) {
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

// reviewConfiguredAttributionMatches reports whether a line is an exactly
// configured attribution. Git treats a trailer key case-insensitively and
// permits whitespace before the colon, so the same trailer must not evade the
// filter by upcasing its key or spacing the colon; the VALUE stays exact, so
// case-distinct evidence is still not this trailer.
func reviewConfiguredAttributionMatches(line string, configuredTrailers []string) bool {
	key, value, ok := reviewConfiguredAttributionParts(line)
	if !ok {
		return false
	}
	for _, trailer := range configuredTrailers {
		view, _ := reviewAttributionView(trailer)
		configuredKey, configuredValue, ok := reviewConfiguredAttributionParts(view)
		if ok && key == configuredKey && value == configuredValue {
			return true
		}
	}
	return false
}

// reviewConfiguredAttributionParts splits one decoded line into the trailer key
// as Git compares it and the value as the configuration spells it.
func reviewConfiguredAttributionParts(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	key = strings.ToLower(strings.Join(strings.Fields(key), " "))
	return key, strings.TrimSpace(value), true
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

func reviewAttributionView(line string) (string, bool) {
	return attributionViewWithLine(line, reviewAttributionLine)
}

func attributionViewWithLine(line string, lineView func(string) (string, bool)) (string, bool) {
	content, continuation := lineView(html.UnescapeString(line))
	parts := strings.Split(content, "\n")
	for i, part := range parts {
		text, indented := lineView(part)
		text = attributionKeyView(text, lineView)
		if prefix := reviewGeneratedWith.FindString(text); prefix != "" {
			value, _ := lineView(text[len(prefix):])
			text = prefix + value
		} else if key, value, ok := strings.Cut(text, ":"); ok {
			separator := ":"
			if strings.TrimLeft(value, " \t\r") != value {
				separator += " "
			}
			for {
				before := value
				value = attributionTextView(value, lineView)
				if value == before {
					break
				}
			}
			text = key + separator + value
		}
		if i > 0 && indented {
			text = " " + text
		}
		parts[i] = text
	}
	return strings.Join(parts, "\n"), continuation
}

func attributionTextView(value string, lineView func(string) (string, bool)) string {
	mailbox := ""
	for end := len(value); end > 0; {
		at := strings.LastIndexAny(value[:end], "<`*_~\"'")
		if at < 0 {
			break
		}
		address, _ := lineView(value[at:])
		if strings.HasPrefix(address, "<") && strings.HasSuffix(address, ">") {
			if _, err := mail.ParseAddress(address); err == nil {
				mailbox = " " + address
				value = value[:at]
				trimmed := strings.TrimRight(value, " \t\r\n.,;:!?")
				if strings.Contains(value[len(trimmed):], "\n") {
					mailbox = "\n " + address
				}
				break
			}
		}
		end = at
	}
	value, _ = lineView(value)
	return value + mailbox
}

func attributionKeyView(text string, lineView func(string) (string, bool)) string {
	for end := strings.IndexByte(text, ':') + 1; end > 0 && end <= len(text); {
		prefix, _ := lineView(text[:end])
		key, _ := lineView(strings.TrimSuffix(prefix, ":"))
		candidate := key + ":"
		if strings.EqualFold(candidate, PreservedHumanCoauthorLabel) {
			return "Co-Authored-By:" + text[end:]
		}
		if reviewTrailerKey.FindString(candidate) == candidate {
			return candidate + text[end:]
		}
		if strings.HasPrefix(text[end:], "</code>") {
			end += len("</code>")
		} else if end < len(text) && strings.ContainsRune("`*_~\"' \t.,;:!?", rune(text[end])) {
			end++
		} else {
			break
		}
	}
	return text
}

func reviewAttributionEntry(lines []string, start int, configuredTrailers []string) (end int, unfolded string, entry, ai bool) {
	return attributionEntryWithView(lines, start, configuredTrailers, reviewAttributionView)
}

func attributionEntryWithView(lines []string, start int, configuredTrailers []string, view func(string) (string, bool)) (end int, unfolded string, entry, ai bool) {
	end = start + 1
	firstLine, _ := view(lines[start])
	unfolded = firstLine
	if end < len(lines) {
		nextLine, continuation := view(lines[end])
		if continuation {
			nextLine = " " + nextLine
		}
		paired, _ := view(lines[start] + "\n" + lines[end])
		first, following, hasPair := strings.Cut(paired, "\n")
		if reviewAttributionStart(unfolded) {
			if joined, ok := reviewAttributionContinuation(unfolded, nextLine); ok {
				unfolded = joined
				end++
			}
		}
		if end == start+1 && hasPair && first != unfolded && reviewAttributionStart(first) {
			if joined, ok := reviewAttributionContinuation(first, following); ok {
				unfolded = joined
				end++
			}
		}
	}
	ai = git.IsAIAttributionTrailer(unfolded) || reviewConfiguredAttributionMatches(firstLine, configuredTrailers) || reviewConfiguredAttributionMatches(unfolded, configuredTrailers)
	key, value, _ := strings.Cut(unfolded, ":")
	if strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") && !strings.EqualFold(strings.TrimSpace(value), "Claude") && !strings.ContainsAny(value, "<>") {
		ai = ai || git.IsAIAttributionTrailer("Assisted-by:"+value)
	}
	entry = ai || (reviewAttributionStart(unfolded) && reviewAttributionAddressComplete(unfolded) && !reviewGeneratedWith.MatchString(unfolded))
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
	subject = reviewFixCommitSubject(body)
	if body == NoChangesAppliedSummary {
		body = "Fixer summary: " + body
	}
	return subject, body
}

const reviewFixSubjectMaxRunes = 100

func reviewFixCommitSubject(body string) string {
	first := ""
	for _, line := range strings.Split(body, "\n") {
		if first = strings.Join(strings.Fields(line), " "); first != "" {
			break
		}
	}
	const trimSet = " \t\r\n\"'.;:,-"
	if runes := []rune(first); len(runes) > reviewFixSubjectMaxRunes {
		first = string(runes[:reviewFixSubjectMaxRunes])
		if cut := strings.LastIndexByte(first, ' '); cut > len(first)/2 {
			first = first[:cut]
		}
	}
	return strings.Trim(first, trimSet)
}

var reviewCoAuthorTrailer = regexp.MustCompile(`(?i)^co-authored-by[ \t]*:`)

func splitTrailingCoAuthors(summary string) (head, trailers string) {
	lines := strings.Split(summary, "\n")
	cut := len(lines)
	coauthor := false
	for i := len(lines); i > 0; i-- {
		line := lines[i-1]
		if reviewTrailerKey.MatchString(line) {
			cut = i - 1
			coauthor = coauthor || reviewCoAuthorTrailer.MatchString(line)
		} else if line == "" || (line[0] != ' ' && line[0] != '\t') {
			break
		}
	}
	head = strings.TrimRight(strings.Join(lines[:cut], "\n"), "\r\n \t")
	if !coauthor {
		return summary, ""
	}
	return head, strings.Join(lines[cut:], "\n")
}

func reviewFixCommitBody(summary, previousFindings string, configuredTrailers []string) string {
	var findings Findings
	unreadable := json.Unmarshal([]byte(previousFindings), &findings) != nil
	if !unreadable && len(findings.Items) == 0 {
		return summary
	}
	head, trailers := splitTrailingCoAuthors(summary)
	var b strings.Builder
	b.WriteString(head)
	if head != "" {
		b.WriteString("\n\n")
	}
	if unreadable {
		b.WriteString("Round findings unavailable (payload could not be read).")
		if trailers != "" {
			b.WriteString("\n\n" + trailers)
		}
		return b.String()
	}
	b.WriteString("Round findings:")
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
	if trailers != "" {
		b.WriteString("\n\n" + trailers)
	}
	return b.String()
}
