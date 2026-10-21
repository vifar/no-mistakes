package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// V2 admits prompts asynchronously. Its event stream is volatile, not a proof
// of completion or of absent tool effects. Wait for the owned execution, then
// read the complete paginated projection before accepting an answer. The V1
// adapter remains separate because its synchronous message contract differs.
func (a *opencodeAgent) runV2(ctx context.Context, opts RunOpts, baseURL string) (*Result, error) {
	client := opencodeV2Client{authorization: a.auth().header()}
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	sessionURL := baseURL + "/api/session/" + url.PathEscape(sessionID)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Both calls target only the fresh session this invocation created.
		// Interrupt before deletion, including ambiguous admissions and cancellation.
		_ = client.json(cleanup, http.MethodPost, sessionURL+"/interrupt", nil, nil)
		_ = client.json(cleanup, http.MethodDelete, sessionURL, nil, nil)
	}()

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, baseURL+"/api/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client.authenticate(req)
	response, err := opencodeV2HTTP.Do(req)
	if err != nil {
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream: expected SSE response, got HTTP %d", response.StatusCode))
	}
	state := opencodeV2Stream{sessionID: sessionID, parts: make(map[string]string), emitted: make(map[string]string), usage: make(map[string]TokenUsage)}
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		state.read(response.Body, opts)
	}()
	stopStream := func() { streamCancel(); _ = response.Body.Close(); <-streamDone }
	defer stopStream()

	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		// V2's tool-using session API has no native JSON-schema format field.
		// Keep tools available, instruct the model, then validate its final text.
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}
	err = client.json(ctx, http.MethodPost, sessionURL+"/prompt", map[string]string{"text": prompt}, nil)
	if err != nil {
		stopStream()
		evidence := opencodeToolsUnknown
		if requestNeverReachedOpencode(err) {
			evidence = opencodeToolsNone
		}
		return resultFromUsage(accumulateUsage(state.usage)), opencodeTurnFailure(evidence, fmt.Errorf("opencode admit prompt: %w", err))
	}
	if err := client.json(ctx, http.MethodPost, baseURL+"/api/experimental/session/"+url.PathEscape(sessionID)+"/wait", nil, nil); err != nil {
		stopStream()
		return resultFromUsage(accumulateUsage(state.usage)), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode wait: %w", err))
	}
	var session struct {
		Data *opencodeV2Session `json:"data"`
	}
	if err := client.json(ctx, http.MethodGet, sessionURL, nil, &session); err != nil {
		stopStream()
		return resultFromUsage(accumulateUsage(state.usage)), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode session outcome: %w", err))
	}
	if session.Data == nil || session.Data.ID != sessionID {
		stopStream()
		return resultFromUsage(accumulateUsage(state.usage)), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode session outcome: missing or mismatched session identity"))
	}
	messages, err := client.messages(ctx, sessionURL)
	stopStream()
	if err != nil {
		return resultFromUsage(accumulateUsage(state.usage)), opencodeTurnFailure(opencodeToolsUnknown, err)
	}
	usage := make(map[string]TokenUsage)
	evidence := opencodeToolsNone
	text := ""
	finalMessageID := ""
	var failure *opencodeV2Error
	for _, message := range messages {
		if message.Tokens != nil {
			usage[message.ID] = opencodeTokensToUsage(message.Tokens)
		}
		if message.Type != "assistant" {
			continue
		}
		var current strings.Builder
		for _, content := range message.Content {
			switch content.Type {
			case "text":
				current.WriteString(content.Text)
			case "reasoning":
			case "tool":
				evidence = opencodeToolsRan
			default:
				return resultFromUsage(accumulateUsage(usage)), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode projection: unsupported assistant content"))
			}
		}
		if message.Error != nil {
			failure = message.Error
		}
		// Only the final provider step is an answer. Earlier commentary and
		// tool-call steps must not substitute for an absent final response.
		text = current.String()
		finalMessageID = message.ID
	}
	total := accumulateUsage(usage)
	if session.Data.Outcome != "succeeded" {
		if failure != nil {
			// V2 no longer exposes V1's isRetryable verdict. An admitted V2
			// execution is not replayed on provider prose or guessed retryability.
			return resultFromUsage(total), &opencodeMessageFailure{name: failure.Type, message: failure.Message, statusCode: failure.Status, toolActivity: evidence == opencodeToolsRan}
		}
		return resultFromUsage(total), &opencodeMessageFailure{name: "execution", message: fmt.Sprintf("execution did not succeed (outcome %q)", session.Data.Outcome), toolActivity: evidence == opencodeToolsRan}
	}
	if len(messages) == 0 {
		return resultFromUsage(total), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode projection: no messages after execution"))
	}
	// Only a contiguous prefix of completed text parts reaches OnChunk.
	// Events can omit entire parts, not just deltas, so reconcile against
	// the complete projection rather than the set of observed stream keys.
	streamed := state.emitted[finalMessageID]
	if !strings.HasPrefix(text, streamed) {
		return resultFromUsage(total), opencodeTurnFailure(opencodeToolsUnknown, fmt.Errorf("opencode projection disagrees with completed streamed text"))
	}
	state.emit(finalMessageID, strings.TrimPrefix(text, streamed), opts)

	result, err := finalizeTextResult("opencode", text, opts.JSONSchema, total)
	return result, opencodeTurnFailure(evidence, err)
}

// The private server credential is sent only to the selected loopback server,
// never followed through a redirect or interpolated into an error URL.
var opencodeV2HTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type opencodeV2Client struct{ authorization string }

func (c opencodeV2Client) authenticate(request *http.Request) {
	if c.authorization != "" {
		request.Header.Set("Authorization", c.authorization)
	}
}

func (c opencodeV2Client) json(ctx context.Context, method, endpoint string, body, target any) error {
	raw, err := c.raw(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if target != nil {
		if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("decode OpenCode v2 response: %w", err)
		}
	}
	return nil
}

func (c opencodeV2Client) raw(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authenticate(req)
	response, err := opencodeV2HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s failed with HTTP %d", method, req.URL.Path, response.StatusCode)
	}
	return raw, nil
}

type opencodeV2Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type opencodeV2Session struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
}

func (c opencodeV2Client) messages(ctx context.Context, sessionURL string) ([]opencodeV2Message, error) {
	query := url.Values{"order": {"asc"}, "limit": {"100"}}
	seen := make(map[string]bool)
	var messages []opencodeV2Message
	for {
		var page struct {
			Data   []opencodeV2Message `json:"data"`
			Cursor *struct {
				Next *string `json:"next"`
			} `json:"cursor"`
		}
		if err := c.json(ctx, http.MethodGet, sessionURL+"/message?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("opencode projection: %w", err)
		}
		if page.Data == nil || page.Cursor == nil {
			return nil, fmt.Errorf("opencode projection: incomplete message page")
		}
		for _, message := range page.Data {
			if message.ID == "" || message.Type == "" {
				return nil, fmt.Errorf("opencode projection: missing message identity or type")
			}
			if message.Type == "assistant" && message.Content == nil {
				return nil, fmt.Errorf("opencode projection: missing assistant content")
			}
		}
		messages = append(messages, page.Data...)
		if page.Cursor.Next == nil {
			return messages, nil
		}
		cursor := *page.Cursor.Next
		if cursor == "" || seen[cursor] {
			return nil, fmt.Errorf("opencode projection: invalid or repeated pagination cursor")
		}
		seen[cursor] = true
		query = url.Values{"cursor": {cursor}, "limit": {"100"}}
	}
}

type opencodeV2Stream struct {
	sessionID     string
	parts         map[string]string
	emitted       map[string]string
	ended         map[string]bool
	nextOrdinal   map[string]int
	lastMessageID string
	usage         map[string]TokenUsage
}

func (s *opencodeV2Stream) emit(messageID, text string, opts RunOpts) {
	if text == "" {
		return
	}
	if opts.OnChunk != nil {
		if s.lastMessageID != "" && s.lastMessageID != messageID {
			opts.OnChunk("\n\n")
		}
		opts.OnChunk(text)
	}
	s.lastMessageID = messageID
	s.emitted[messageID] += text
}

func (s *opencodeV2Stream) read(reader io.Reader, opts RunOpts) {
	if s.ended == nil {
		s.ended = make(map[string]bool)
	}
	if s.nextOrdinal == nil {
		s.nextOrdinal = make(map[string]int)
	}
	progress := agentProgressEmitter(opts, "opencode")
	_ = parseSSE(reader, func(event sseEvent) bool {
		var value struct {
			Type string `json:"type"`
			Data struct {
				SessionID string          `json:"sessionID"`
				ToolID    string          `json:"id"`
				MessageID string          `json:"assistantMessageID"`
				Ordinal   int             `json:"ordinal"`
				Text      string          `json:"text"`
				Delta     string          `json:"delta"`
				Tokens    *opencodeTokens `json:"tokens"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(event.Data), &value) != nil || value.Data.SessionID != s.sessionID {
			return true
		}
		data := value.Data
		key := fmt.Sprintf("%s:%d", data.MessageID, data.Ordinal)
		switch value.Type {
		case "session.text.delta":
			// Buffer deltas per part instead of emitting them live: a partial
			// that later turns out not to be a prefix of the complete text
			// cannot be retracted from the append-only OnChunk interface.
			s.parts[key] += data.Delta
			if data.Delta != "" {
				emitLifecycle(opts, LifecycleEvent{
					Agent:   "opencode",
					Phase:   LifecyclePhaseProgress,
					Message: "opencode producing output",
				})
			}
		case "session.text.ended":
			s.parts[key] = data.Text
			s.ended[key] = true
			// OnChunk is append-only. Hold later parts until all earlier
			// ordinals are complete, so missing or reordered events cannot
			// publish a suffix that the final projection cannot prepend to.
			for {
				ordinal := s.nextOrdinal[data.MessageID]
				partKey := fmt.Sprintf("%s:%d", data.MessageID, ordinal)
				if !s.ended[partKey] {
					break
				}
				s.emit(data.MessageID, s.parts[partKey], opts)
				s.nextOrdinal[data.MessageID] = ordinal + 1
			}
		case "session.tool.called", "session.tool.success", "session.tool.failed":
			if data.MessageID != "" && data.ToolID != "" && progress != nil {
				progress()
			}
		case "session.step.ended", "session.step.failed":
			if data.Tokens != nil && data.MessageID != "" {
				s.usage[data.MessageID] = opencodeTokensToUsage(data.Tokens)
			}
		}
		return true
	})
}
