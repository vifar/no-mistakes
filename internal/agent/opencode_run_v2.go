package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// runOnceV2 runs one turn against an OpenCode 2.x server.
//
// The 2.x protocol differs from 1.x in four ways that matter here:
//
//  1. The session body carries `location` and `permissions` instead of
//     `directory` and `permission` (see sessionCreateBody).
//
//  2. The prompt endpoint does NOT block: it admits the prompt and returns
//     the queued inbox item while the turn runs in the background. The
//     assistant's message is therefore read back from the message list
//     after the event stream reports the turn finished.
//
//  3. The event frames carry a top-level `type`, and the completion signal
//     is `session.execution.succeeded` instead of `session.idle`.
//
//  4. There is no json_schema output format on either endpoint, so the
//     native structured-output route does not exist here: the prompt-only
//     route (schema in the prompt, JSON validated on the way out) is the
//     only structured-output path, and it is the same route 1.x already
//     uses when its native format is rejected.
func (a *opencodeAgent) runOnceV2(ctx context.Context, opts RunOpts, baseURL string, nativeFormat bool) (*Result, error) {
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// The schema rides the prompt on 2.x, because the native json_schema
	// format is not reachable through this API. The prompt-only route is
	// therefore the 2.x structured-output path, whether or not the caller
	// asked for the native format.
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	state := &opencodeStreamState{
		sessionID:  sessionID,
		onChunk:    opts.OnChunk,
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	// The prompt is admitted on its own goroutine because the 2.x endpoint
	// answers immediately: the turn's work happens while the stream runs.
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan opencodeMessageResult, 1)
	go func() {
		_, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, nil)
		msgCh <- opencodeMessageResult{err: err, settled: true}
	}()

	streamErr := parseOpencodeV2SSE(eventBody, state)
	streamCancel()

	// The prompt request is the only thing that could have failed on its
	// own; the turn itself is reported through the stream.
	mr := pollOpencodeMessage(msgCh)
	if !mr.settled {
		mr = awaitOpencodeMessage(msgCh, opencodeEvidenceWait)
	}
	if mr.err != nil {
		// The prompt never reached opencode, so no tool of this turn can
		// have run: the turn is safe to report as failed without the
		// replay gate.
		return resultFromUsage(state.usage), fmt.Errorf("opencode message: %w", mr.err)
	}

	if streamErr != nil {
		var execErr *opencodeV2ExecutionError
		if asOpencodeV2ExecutionError(streamErr, &execErr) {
			// The server reported why the turn failed. A tool part in the
			// stream is the evidence that the turn may have left side
			// effects, which is what the replay gate reads.
			evidence := state.v2ToolEvidence()
			return resultFromUsage(state.usage), opencodeTurnFailure(evidence, streamErr)
		}
		return resultFromUsage(state.usage), opencodeTurnFailure(state.v2ToolEvidence(), fmt.Errorf("opencode events: %w", streamErr))
	}

	// The turn is over: read the assistant's message back from the session.
	resp, err := a.readAssistantMessage(ctx, baseURL, sessionID)
	if err != nil {
		return resultFromUsage(state.usage), opencodeTurnFailure(state.v2ToolEvidence(), fmt.Errorf("opencode message: %w", err))
	}
	if resp == nil {
		return resultFromUsage(state.usage), opencodeTurnFailure(state.v2ToolEvidence(), errors.New("opencode returned no message"))
	}

	// A failed turn reports its cause on the message, not on the HTTP
	// status, so the request itself looks successful. Surface that error
	// instead of falling through to the streamed text: opencode leaves no
	// usable text behind a failed turn.
	if resp.Info != nil && resp.Info.Error != nil {
		evidence := state.v2ToolEvidence()
		return resultFromUsage(state.usage), opencodeTurnFailure(evidence, newOpencodeMessageFailure(resp.Info.Error, evidence == opencodeToolsRan))
	}

	// Prefer the message's own text; the stream already emitted the deltas
	// as they arrived.
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	if outputText == "" {
		for _, part := range resp.Parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				outputText = part.Text
				break
			}
		}
	}
	if outputText == "" {
		for _, part := range resp.Parts {
			if part.Type == "text" {
				outputText = part.Text
				break
			}
		}
	}
	if resp.Info != nil && resp.Info.Tokens != nil {
		// The 2.x stream already reported the turn's usage on
		// session.step.ended, and the message reports the same tokens. The
		// message is the authoritative final record, so it REPLACES the
		// stream's figure rather than adding to it: the two describe the
		// same turn, and adding them would double-count it.
		state.usageByMsg = map[string]TokenUsage{resp.Info.ID: opencodeTokensToUsage(resp.Info.Tokens)}
		state.usage = accumulateUsage(state.usageByMsg)
	}

	result, err := finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
	if err != nil {
		return result, opencodeTurnFailure(state.v2ToolEvidence(), err)
	}
	return result, nil
}

// v2ToolEvidence answers the replay gate's question for a 2.x turn.
//
// The 2.x stream names tool activity explicitly (session.tool.called and
// friends), so a tool that ran is a fact. The absence of one is NOT a proof
// that none ran: the stream is the only record of the turn's tool parts, so
// an unobserved turn stays UNKNOWN and fails closed, exactly like the 1.x
// path does.
func (s *opencodeStreamState) v2ToolEvidence() opencodeToolEvidence {
	if s.toolInvoked {
		return opencodeToolsRan
	}
	return opencodeToolsUnknown
}

// asOpencodeV2ExecutionError reports whether err is a 2.x execution failure.
func asOpencodeV2ExecutionError(err error, target **opencodeV2ExecutionError) bool {
	execErr, ok := err.(*opencodeV2ExecutionError)
	if !ok {
		return false
	}
	*target = execErr
	return true
}
