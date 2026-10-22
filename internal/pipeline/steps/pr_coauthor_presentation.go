package steps

import (
	"html"
	"net/mail"
	"strings"
)

func presentPRCoauthors(text string) string {
	lines := strings.Split(text, "\n")
	decoded := make([]string, len(lines))
	for i, line := range lines {
		decoded[i] = html.UnescapeString(line)
	}
	var kept []string
	for start := 0; start < len(lines); {
		end, unfolded, entry, ai := reviewAttributionEntry(decoded, start, nil)
		if !ai {
			key, value, _ := strings.Cut(unfolded, ":")
			if entry && strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") {
				if i := strings.LastIndex(value, "<"); i >= 0 {
					if _, err := mail.ParseAddress(strings.TrimSpace(value[i:])); err == nil {
						line := lines[start]
						label := strings.Index(strings.ToLower(line), "co-authored-by")
						if label >= 0 {
							if literalKey := reviewTrailerKey.FindString(line[label:]); literalKey != "" {
								lines[start] = line[:label] + "preserved human co-author:" + line[label+len(literalKey):]
							}
						}
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
