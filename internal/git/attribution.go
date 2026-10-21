package git

import (
	"net/mail"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
)

var attributionTrailerKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*[ \t]*:`)
var generatedAttribution = regexp.MustCompile(`(?i)^(?:🤖[ \t]*)?generated[ \t]+with[ \t]+(.+)$`)
var modelAttributionIdentity = regexp.MustCompile(`^(?:gpt-[0-9][a-z0-9.-]*|o[134](?:-[a-z0-9.-]+)?|claude-(?:opus|sonnet|haiku)-[a-z0-9.-]+|gemini-[0-9][a-z0-9.-]*)$`)

var attributionLink = regexp.MustCompile(`^\[([^]]+)\]\(https?://[^)]+\)$`)

// IsAIAttributionTrailer classifies a complete, unfolded attribution line.
// Names are matched as identities, never substrings: Claude Smith and prose
// mentioning generated fixtures are human attribution and disclosure, respectively.
// Kept public so summary writers can use the same rule BEFORE flattening lines.
func IsAIAttributionTrailer(line string) bool {
	line = strings.TrimSpace(line)
	if match := generatedAttribution.FindStringSubmatch(line); match != nil {
		name := match[1]
		if link := attributionLink.FindStringSubmatch(name); link != nil {
			name = link[1]
		}
		return aiAttributionIdentity(name)
	}
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "claude-session", "codex-session":
		return true
	case "co-authored-by":
		address, err := attributionAddress(value)
		if err != nil {
			return false
		}
		// Claude is also a human first name. A bare name needs the known
		// tool mailbox; a personal address must keep its human attribution.
		if strings.EqualFold(address.Name, "Claude") {
			return strings.EqualFold(address.Address, "noreply@anthropic.com")
		}
		return aiAttributionIdentity(address.Name)
	case "assisted-by":
		address, err := attributionAddress(value)
		if err == nil && address.Name != "" {
			if strings.EqualFold(address.Name, "Claude") {
				return strings.EqualFold(address.Address, "noreply@anthropic.com")
			}
			return aiAttributionIdentity(address.Name)
		}
		if aiAttributionIdentity(value) {
			return true
		}
		fields := strings.Fields(value)
		return len(fields) > 0 && strings.Contains(fields[0], ":") && !strings.ContainsAny(value, "<>") && aiAttributionIdentity(fields[0])
	}
	return false
}

func aiAttributionIdentity(name string) bool {
	name = strings.ToLower(strings.Join(strings.Fields(name), " "))
	if modelAttributionIdentity.MatchString(name) {
		return true
	}
	switch name {
	case "claude", "claude code", "codex", "chatgpt", "openai", "anthropic",
		"copilot", "github copilot", "gemini", "grok", "pi", "omp", "opencode", "no-mistakes",
		"antigravity", "rovodev", "cursor", "devin":
		return true
	}
	if strings.HasPrefix(name, "no-mistakes ") {
		return aiAttributionIdentity(strings.TrimPrefix(name, "no-mistakes "))
	}
	if strings.HasPrefix(name, "acp:") {
		parts := strings.Split(name, ":")
		return len(parts) >= 2 && parts[1] != "" && !strings.ContainsAny(name, " \t<>")
	}
	if identity, _, ok := strings.Cut(name, ":"); ok && !strings.ContainsAny(name, " \t<>") {
		return aiAttributionIdentity(identity)
	}
	return false
}

func attributionAddress(value string) (*mail.Address, error) {
	value = strings.TrimSpace(value)
	if i := strings.LastIndex(value, " <"); i >= 0 && strings.HasSuffix(value, ">") {
		name := value[:i]
		if !strings.HasPrefix(name, "\"") {
			value = "\"" + strings.ReplaceAll(strings.ReplaceAll(name, "\\", "\\\\"), "\"", "\\\"") + "\"" + value[i:]
		}
	}
	return mail.ParseAddress(value)
}

// StripAIAttributionTrailers removes AI attribution only from trailing trailer
// paragraphs. The subject, prose, human trailers and their order survive.
// Generated-with footers are recognized alongside Git's key:value trailers.
// Continuations belong to their trailer, so deleting one cannot orphan its value.
func StripAIAttributionTrailers(message string, configured ...string) string {
	return stripAttribution(message, configured, nil)
}

func stripAttribution(message string, configured []string, preserved map[string]int) string {
	lines := strings.Split(message, "\n")
	drop := make([]bool, len(lines))
	changed := false
	end := len(lines)
	for end > 1 {
		for end > 1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		start := end - 1
		for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
			start--
		}
		if start == 0 {
			break
		} // Never interpret a subject as a trailer.
		type trailer struct {
			start, end int
			text       string
		}
		var entries []trailer
		valid := true
		for i := start; i < end; i++ {
			line := lines[i]
			if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(entries) > 0 {
				entry := &entries[len(entries)-1]
				entry.text += " " + strings.TrimSpace(line)
				entry.end = i + 1
			} else if attributionTrailerKey.MatchString(line) || generatedAttribution.MatchString(line) {
				entries = append(entries, trailer{i, i + 1, line})
			} else {
				valid = false
				break
			}
		}
		if !valid {
			break
		}
		allRemoved := true
		for _, entry := range entries {
			if !IsAIAttributionTrailer(entry.text) && !configuredAttribution(entry.text, configured) {
				allRemoved = false
				continue
			}
			if preserved != nil {
				if preserved["*"] < 0 {
					preserved[entry.text]++
					allRemoved = false
					continue
				}
				if preserved[entry.text] > 0 {
					preserved[entry.text]--
					allRemoved = false
					continue
				}
			}
			changed = true
			for i := entry.start; i < entry.end; i++ {
				drop[i] = true
			}
		}
		if allRemoved {
			for i := start - 1; i > 0 && strings.TrimSpace(lines[i]) == ""; i-- {
				drop[i] = true
			}
		}
		end = start
	}
	if !changed {
		return message
	}
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\r\n")
}

func configuredAttribution(line string, sources []string) bool {
	for _, source := range sources {
		tmpl, err := template.New("attribution").Parse(strings.TrimSpace(source))
		if err != nil || tmpl.Tree == nil || tmpl.Tree.Root == nil {
			continue
		}
		pattern := "^"
		valid := true
		for _, node := range tmpl.Tree.Root.Nodes {
			switch node := node.(type) {
			case *parse.TextNode:
				pattern += regexp.QuoteMeta(string(node.Text))
			case *parse.ActionNode:
				if node.String() != "{{.Agent}}" && node.String() != "{{.Model}}" {
					valid = false
					break
				}
				pattern += `[A-Za-z0-9_.:/-]{1,64}`
			default:
				valid = false
			}
		}
		if valid {
			match, err := regexp.MatchString(pattern+"$", line)
			if err == nil && match {
				return true
			}
		}
	}
	return false
}

func stripAttributionPreserving(message, original string, configured []string) string {
	var preserved map[string]int
	if original != "" {
		preserved = map[string]int{"*": -1}
		stripAttribution(original, configured, preserved)
		delete(preserved, "*")
	}
	return stripAttribution(message, configured, preserved)
}
