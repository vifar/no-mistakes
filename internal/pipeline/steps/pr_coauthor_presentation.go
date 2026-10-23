package steps

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PreservedHumanCoauthorLabel is the published label for a preserved human
// co-author. The commit keeps the trailer verbatim; only the published body
// renders it as a labelled field, so no text check can mistake the preserved
// human co-author for AI attribution.
//
// It is the same label the shared publication rule renders (main's
// "co-author (quoted):"), so the published body carries ONE form of it
// whichever path published it.
const PreservedHumanCoauthorLabel = "co-author (quoted):"

// prAIAttributionNotice is what a published generated section shows in place
// of an AI attribution line. The line is broken, not dropped: the evidence
// still shows that the source carried an attribution, and no AI identity
// survives in the published body.
const prAIAttributionNotice = "<no-mistakes: AI attribution line neutralized>"

// prDisplayText renders one published text block: AI attribution and the
// exactly configured trailers are removed, a preserved human co-author becomes a
// labelled field, and ordinary prose is left alone.
//
// A recorded payload keeps its line breaks as the two-character escape, so its
// entries are one line to a line-based scanner. Such a line is filtered through
// its own string values, and the payload keeps the bytes it was recorded with.
func prDisplayText(text string, configuredTrailers ...string) string {
	return prDisplayTextMode(text, false, configuredTrailers...)
}

// prDisplayNoticeText keeps the generated-section contract for a block whose
// reader can verify the behaviour from the body: an AI attribution line is
// replaced by the neutralization notice instead of being silently removed.
func prDisplayNoticeText(text string, configuredTrailers ...string) string {
	return prDisplayTextMode(text, true, configuredTrailers...)
}

func prDisplayTextMode(text string, notice bool, configuredTrailers ...string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	jsonFence := false
	for start := 0; start < len(lines); {
		if fence := strings.TrimSpace(lines[start]); strings.HasPrefix(fence, "```") {
			jsonFence = !jsonFence && fence == "```json"
		}
		if payload, tail, end, ok := recordedPayloadBlock(lines, start, jsonFence); ok {
			kept = append(kept, filterRecordedPayload(payload, notice, configuredTrailers...)+tail)
			start = end
			continue
		}
		end, unfolded, entry, ai := reviewAttributionEntry(lines, start, configuredTrailers)
		if !ai {
			key, _, _ := strings.Cut(unfolded, ":")
			if entry && strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") && reviewAttributionAddressComplete(unfolded) {
				lines[start] = presentHumanCoauthor(lines[start])
			}
			kept = append(kept, lines[start:end]...)
		} else if notice {
			kept = append(kept, prAIAttributionNotice)
		} else {
			kept = append(kept, "")
		}
		start = end
	}
	return strings.Join(kept, "\n")
}

// presentHumanCoauthor renders a preserved human co-author line as a labelled
// field. The line keeps every byte it carried except the trailer key, so the
// name and the address stay visible to a reader.
func presentHumanCoauthor(line string) string {
	label := strings.Index(strings.ToLower(line), "co-authored-by")
	if label < 0 {
		return line
	}
	literalKey := reviewTrailerKey.FindString(line[label:])
	if literalKey == "" {
		return line
	}
	return line[:label] + PreservedHumanCoauthorLabel + line[label+len(literalKey):]
}

func recordedPayloadBlock(lines []string, start int, jsonFence bool) (string, string, int, bool) {
	line := lines[start]
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", start, false
	}
	rest := strings.Join(lines[start:], "\n")
	offset := 0
	if trimmed[0] == '{' || trimmed[0] == '[' {
		next := strings.TrimSpace(trimmed[1:])
		if next != "" {
			allowed := "\"}"
			if trimmed[0] == '[' {
				allowed = "\"{[]-0123456789"
			}
			if !strings.ContainsRune(allowed, rune(next[0])) && !(trimmed[0] == '[' && (strings.HasPrefix(next, "true") || strings.HasPrefix(next, "false") || strings.HasPrefix(next, "null"))) {
				return "", "", start, false
			}
		}
		decoder := json.NewDecoder(strings.NewReader(rest))
		var payload json.RawMessage
		if err := decoder.Decode(&payload); err == nil {
			offset = int(decoder.InputOffset())
		} else {
			var stack []byte
			data := []byte(rest)
			offset = len(rest)
			for at := 0; at < len(rest); at++ {
				if at > 0 && rest[at-1] == '\n' && strings.HasPrefix(strings.TrimLeft(rest[at:], " \t\r"), "```") {
					offset = at - 1
					break
				}
				if rest[at] == '"' {
					if end, ok := jsonStringEnd(data, at); ok {
						at = end - 1
						continue
					}
					if newline := strings.IndexByte(rest[at:], '\n'); newline >= 0 {
						at += newline - 1
						continue
					}
					break
				}
				switch rest[at] {
				case '{':
					stack = append(stack, '}')
				case '[':
					stack = append(stack, ']')
				case '}', ']':
					if len(stack) > 0 && stack[len(stack)-1] == rest[at] {
						stack = stack[:len(stack)-1]
					}
					if len(stack) == 0 {
						offset = at + 1
					}
				}
				if offset != len(rest) {
					break
				}
			}
		}
		if trimmed[0] == '[' && strings.HasPrefix(strings.TrimLeft(rest[offset:], " \t"), "(") {
			return "", "", start, false
		}
	} else if trimmed[0] == '"' {
		end, ok := jsonStringEnd([]byte(line), strings.IndexByte(line, '"'))
		if !ok {
			return "", "", start, false
		}
		after := strings.TrimSpace(line[end:])
		if strings.HasPrefix(after, ":") {
			decoder := json.NewDecoder(strings.NewReader("{" + line))
			if _, err := decoder.Token(); err != nil {
				return "", "", start, false
			}
			for decoder.More() {
				if _, err := decoder.Token(); err != nil {
					break
				}
				var value json.RawMessage
				if err := decoder.Decode(&value); err != nil {
					break
				}
				offset = int(decoder.InputOffset()) - 1
			}
		} else if jsonFence || (after != "" && strings.Trim(after, ",]} \t\r") == "") || (after == "" && start+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[start+1]), "]")) {
			offset = end
		}
	}
	if offset == 0 {
		return "", "", start, false
	}
	lineEnd := strings.IndexByte(rest[offset:], '\n')
	if lineEnd < 0 {
		lineEnd = len(rest)
	} else {
		lineEnd += offset
	}
	end := start + strings.Count(rest[:lineEnd], "\n") + 1
	return rest[:offset], rest[offset:lineEnd], end, true
}

// filterRecordedPayload filters the string values inside a recorded payload and
// splices the filtered text back into the payload's own bytes, so the published
// evidence keeps the shape it was recorded with.
func filterRecordedPayload(line string, notice bool, configuredTrailers ...string) string {
	out := []byte(line)
	for offset := 0; offset < len(out); {
		start := bytes.IndexByte(out[offset:], '"')
		if start < 0 {
			break
		}
		start += offset
		slashes := 0
		for previous := start - 1; previous >= 0 && out[previous] == '\\'; previous-- {
			slashes++
		}
		if slashes%2 != 0 {
			offset = start + 1
			continue
		}
		end, ok := jsonStringEnd(out, start)
		if !ok {
			offset = start + 1
			continue
		}
		next := end
		for next < len(out) && strings.ContainsRune(" \t\r\n", rune(out[next])) {
			next++
		}
		if next < len(out) && out[next] == ':' {
			offset = end
			continue
		}
		literal := string(out[start:end])
		var value string
		if err := json.Unmarshal([]byte(literal), &value); err == nil {
			if filtered := prDisplayTextMode(value, notice, configuredTrailers...); filtered != value {
				if encoded, err := json.Marshal(filtered); err == nil {
					rebuilt := make([]byte, 0, len(out))
					rebuilt = append(rebuilt, out[:start]...)
					rebuilt = append(rebuilt, encoded...)
					rebuilt = append(rebuilt, out[end:]...)
					out = rebuilt
					offset = start + len(encoded)
					continue
				}
			}
		}
		offset = start + 1
	}
	return string(out)
}

// jsonStringEnd returns the end of the JSON string literal that starts at the
// opening quote, and whether the literal is complete.
func jsonStringEnd(data []byte, start int) (int, bool) {
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '\n', '\r':
			return 0, false
		case '"':
			return i + 1, true
		}
	}
	return 0, false
}

func prDisplayFindings(raw types.Findings, configuredTrailers ...string) types.Findings {
	clean := func(text string) string { return prDisplayNoticeText(text, configuredTrailers...) }
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

// prDisplayBody normalises a whole published PR body. The body is assembled
// from several owners — the agent's narrative, the recorded evidence and the
// generated appendix — and a preserved human co-author can reach any of them,
// so the body is filtered as one text block: every published copy of the
// preserved human co-author becomes a labelled field, AI attribution and the
// exactly configured trailers are removed, and ordinary prose is left alone.
func prDisplayBody(body string, configuredTrailers ...string) string {
	return prDisplayText(body, configuredTrailers...)
}
