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
	for start := 0; start < len(lines); {
		if payload, end, ok := recordedPayloadBlock(lines, start); ok {
			kept = append(kept, filterRecordedPayload(payload, notice, false, configuredTrailers...))
			start = end
			continue
		}
		end, unfolded, entry, ai := reviewAttributionEntry(lines, start, configuredTrailers)
		if ai {
			if notice {
				kept = append(kept, prAIAttributionNotice)
			} else {
				kept = append(kept, "")
			}
			start = end
			continue
		}
		key, _, _ := strings.Cut(unfolded, ":")
		switch {
		case entry && strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") && reviewAttributionAddressComplete(unfolded):
			lines[start] = presentHumanCoauthor(lines[start])
		case recordedFragmentLine(lines[start]):
			lines[start] = filterRecordedPayload(lines[start], notice, true, configuredTrailers...)
		}
		kept = append(kept, lines[start:end]...)
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

func recordedPayloadBlock(lines []string, start int) (string, int, bool) {
	trimmed := strings.TrimSpace(lines[start])
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", start, false
	}
	rest := strings.Join(lines[start:], "\n")
	decoder := json.NewDecoder(strings.NewReader(rest))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return "", start, false
	}
	offset := int(decoder.InputOffset())
	lineEnd := strings.IndexByte(rest[offset:], '\n')
	if lineEnd < 0 {
		lineEnd = len(rest)
	} else {
		lineEnd += offset
	}
	payload := rest[:lineEnd]
	if !json.Valid([]byte(strings.TrimSpace(payload))) {
		return "", start, false
	}
	end := start + strings.Count(rest[:lineEnd], "\n") + 1
	return payload, end, true
}

// filterRecordedPayload filters the string values and keys inside a recorded
// payload and splices the filtered text back into the payload's own bytes, so
// the published evidence keeps the shape it was recorded with.
//
// A fragment is a line of a payload that is not itself a complete document
// (the evidence was cut, or the document continues on other lines). A bare
// quoted element there that quotes the published label is a mention of the
// rendered field and stays, unless it is a complete AI entry: an AI co-author
// is removed from every published form, cut or whole.
func filterRecordedPayload(line string, notice, fragment bool, configuredTrailers ...string) string {
	out := []byte(line)
	for offset := 0; offset < len(out); {
		start := bytes.IndexByte(out[offset:], '"')
		if start < 0 {
			break
		}
		start += offset
		end, ok := jsonStringEnd(out, start)
		if !ok {
			break
		}
		literal := string(out[start:end])
		var value string
		if err := json.Unmarshal([]byte(literal), &value); err == nil && !(fragment && bareQuotedMention(out, start, end, value, configuredTrailers)) {
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
		offset = end
	}
	return string(out)
}

// bareQuotedMention reports whether the string at out[start:end] is a bare
// element (neither a key nor a member value) that quotes the published label
// without being a complete AI attribution entry.
func bareQuotedMention(out []byte, start, end int, value string, configuredTrailers []string) bool {
	if !strings.Contains(strings.ToLower(value), PreservedHumanCoauthorLabel) {
		return false
	}
	if _, _, _, ai := reviewAttributionEntry([]string{value}, 0, configuredTrailers); ai {
		return false
	}
	before := bytes.TrimRight(out[:start], " \t")
	after := bytes.TrimLeft(out[end:], " \t")
	return !bytes.HasSuffix(before, []byte(":")) && !bytes.HasPrefix(after, []byte(":"))
}

// recordedFragmentLine reports whether a line is made only of JSON tokens:
// strings, scalars and structural punctuation. Prose, Markdown and an
// incomplete string are not, so only a line a payload could have produced is
// filtered as one.
func recordedFragmentLine(line string) bool {
	data := []byte(line)
	sawString := false
	for i := 0; i < len(data); {
		switch c := data[i]; {
		case c == ' ' || c == '\t' || c == '\r' || strings.IndexByte("{}[],:", c) >= 0:
			i++
		case c == '"':
			end, ok := jsonStringEnd(data, i)
			if !ok {
				return false
			}
			sawString = true
			i = end
		default:
			end := i
			for end < len(data) && strings.IndexByte(" \t\r{}[],:\"", data[end]) < 0 {
				end++
			}
			if !json.Valid(data[i:end]) {
				return false
			}
			i = end
		}
	}
	return sawString
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
