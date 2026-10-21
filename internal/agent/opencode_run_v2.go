package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// V2 admits prompts asynchronously. Its event stream is volatile, not a proof
// of completion or of absent tool effects. Wait for the owned execution, then
// read the complete paginated projection before accepting an answer. The V1
// adapter remains separate because its synchronous message contract differs.
func (a *opencodeAgent) runOnceV2(ctx context.Context, opts RunOpts, baseURL string) (*Result, error) {
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
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream request: %w", err))
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
	if state.toolInvoked {
		evidence = opencodeToolsRan
	}
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
