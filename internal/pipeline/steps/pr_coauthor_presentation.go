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

// prDisplayText renders one published text block: AI attribution and the
// exactly configured trailers are removed, a preserved human co-author becomes a
// labelled field, and ordinary prose is left alone.
//
// A recorded payload keeps its line breaks as the two-character escape, so its
// entries are one line to a line-based scanner. Such a line is filtered through
// its own string values, and the payload keeps the bytes it was recorded with.
func prDisplayText(text string, configuredTrailers ...string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	for start := 0; start < len(lines); {
		if payload, ok := recordedPayloadLine(lines[start]); ok {
			kept = append(kept, filterRecordedPayload(payload, configuredTrailers...))
			start++
			continue
		}
		end, unfolded, entry, ai := reviewAttributionEntry(lines, start, configuredTrailers)
		if !ai {
			key, _, _ := strings.Cut(unfolded, ":")
			if entry && strings.EqualFold(strings.TrimSpace(key), "Co-Authored-By") && reviewAttributionAddressComplete(unfolded) {
				lines[start] = presentHumanCoauthor(lines[start])
			}
			kept = append(kept, lines[start:end]...)
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

// recordedPayloadLine reports whether a line is a recorded JSON payload: a
// document whose string values keep their line breaks as the two-character
// escape, so its entries are one line to a line-based scanner.
func recordedPayloadLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false
	}
	if !strings.Contains(line, `\n`) {
		return "", false
	}
	if !json.Valid([]byte(trimmed)) {
		return "", false
	}
	return line, true
}

// filterRecordedPayload filters the string values inside a recorded payload and
// splices the filtered text back into the payload's own bytes, so the published
// evidence keeps the shape it was recorded with.
func filterRecordedPayload(line string, configuredTrailers ...string) string {
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
		if err := json.Unmarshal([]byte(literal), &value); err == nil {
			if filtered := prDisplayText(value, configuredTrailers...); filtered != value {
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

// jsonStringEnd returns the end of the JSON string literal that starts at the
// opening quote, and whether the literal is complete.
func jsonStringEnd(data []byte, start int) (int, bool) {
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return 0, false
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

// prDisplayBody normalises a whole published PR body. The body is assembled
// from several owners — the agent's narrative, the recorded evidence and the
// generated appendix — and a preserved human co-author can reach any of them,
// so the body is filtered as one text block: every published copy of the
// preserved human co-author becomes a labelled field, AI attribution and the
// exactly configured trailers are removed, and ordinary prose is left alone.
func prDisplayBody(body string, configuredTrailers ...string) string {
	return prDisplayText(body, configuredTrailers...)
}
