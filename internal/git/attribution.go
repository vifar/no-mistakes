package git

import (
	"net/mail"
	"regexp"
	"strings"
)

var attributionTrailerKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*[ \t]*:`)
var generatedAttribution = regexp.MustCompile(`(?i)^(?:🤖[ \t]*)?generated[ \t]+with[ \t]+(.+)$`)
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
		address, err := mail.ParseAddress(strings.TrimSpace(value))
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
		// Configured assistants use agent:model or no-mistakes:agent:model.
		identity, _, _ := strings.Cut(strings.TrimSpace(value), ":")
		if aiAttributionIdentity(identity) {
			return true
		}
		address, err := mail.ParseAddress(strings.TrimSpace(value))
		return err == nil && aiAttributionIdentity(address.Name)
	}
	return false
}

func aiAttributionIdentity(name string) bool {
	name = strings.ToLower(strings.Join(strings.Fields(name), " "))
	switch name {
	case "claude", "claude code", "codex", "chatgpt", "openai", "anthropic",
		"copilot", "github copilot", "gemini", "grok", "pi", "omp", "opencode", "no-mistakes":
		return true
	}
	// The configured co-author template prefixes the actual provider with
	// no-mistakes. A person's name containing a provider name never matches.
	return strings.HasPrefix(name, "no-mistakes ") && aiAttributionIdentity(strings.TrimPrefix(name, "no-mistakes "))
}

// StripAIAttributionTrailers removes AI attribution only from trailing trailer
// paragraphs. The subject, prose, human trailers and their order survive.
// Generated-with footers are recognized alongside Git's key:value trailers.
// Continuations belong to their trailer, so deleting one cannot orphan its value.
func StripAIAttributionTrailers(message string) string {
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
			if !IsAIAttributionTrailer(entry.text) {
				allRemoved = false
				continue
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
