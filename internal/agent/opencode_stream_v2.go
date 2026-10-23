package agent

import (
	"encoding/json"
	"io"
	"strconv"
)

// opencodeV2Event is one frame of the 2.x event stream.
//
// The 2.x stream is NOT the 1.x stream with different route names: a frame
// carries a top-level `type` and a `data` object, where 1.x carried
// `payload.type` and `payload.properties`. The event NAMES are different
// too: 1.x's completion signal was `session.idle`, and 2.x's is
// `session.execution.succeeded` (with `session.execution.failed` and
// `session.execution.interrupted` as its failures).
type opencodeV2Event struct {
	Type string              `json:"type"`
	Data opencodeV2EventData `json:"data"`
}

// opencodeV2EventData is the payload of a 2.x event.
type opencodeV2EventData struct {
	SessionID string `json:"sessionID"`

	// Text deltas: the assistant's answer arrives as session.text.delta.
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Delta              string `json:"delta"`
	Text               string `json:"text"`

	// Tool activity: a tool part means the turn may have left side effects.
	ToolID   string `json:"id"`
	ToolName string `json:"name"`

	// Usage and completion.
	Tokens *opencodeTokens `json:"tokens"`
	Cost   float64         `json:"cost"`
	Finish string          `json:"finish"`

	// Error: the reason an execution failed.
	Error *opencodeV2Error `json:"error"`
}

// parseOpencodeV2SSE processes the 2.x event stream.
//
// The handler is the same contract as the 1.x parser: it returns false to
// stop the stream. The completion signal is session.execution.succeeded, and
// the failure signals are session.execution.failed and
// session.execution.interrupted.
func parseOpencodeV2SSE(r interface{ Read([]byte) (int, error) }, state *opencodeStreamState) error {
	var sawDone bool
	var streamErr error
	err := parseSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}

		var event opencodeV2Event
		if err := json.Unmarshal([]byte(ev.Data), &event); err != nil {
			return true // skip malformed events
		}
		data := event.Data

		// Filter by session ID. A server-wide stream carries every session's
		// events, so another session's turn must never leak into this one.
		if data.SessionID != "" && data.SessionID != state.sessionID {
			return true
		}

		switch event.Type {
		case "session.execution.succeeded":
			state.terminalFrame = true
			sawDone = true
			return false

		case "session.execution.failed", "session.execution.interrupted":
			// The turn is over: nothing after this frame can describe it, so
			// the stream is a complete record and its tool parts are the whole
			// truth about what ran.
			state.terminalFrame = true
			if data.Error != nil {
				streamErr = &opencodeV2ExecutionError{event: event.Type, err: data.Error}
			} else {
				streamErr = &opencodeV2ExecutionError{event: event.Type, err: &opencodeV2Error{Type: event.Type}}
			}
			return false

		case "session.text.started":
			// The answer for this ordinal starts here.
			state.beginV2Text(data.AssistantMessageID, data.Ordinal)

		case "session.text.delta":
			state.appendV2Text(data.AssistantMessageID, data.Ordinal, data.Delta)

		case "session.text.ended":
			state.endV2Text(data.AssistantMessageID, data.Ordinal, data.Text)

		case "session.tool.called", "session.tool.input.started", "session.tool.progress":
			// A tool part means the turn may have left side effects behind,
			// which is the durable evidence the replay gate reads.
			state.toolInvoked = true

		case "session.step.ended", "session.step.failed":
			if data.Tokens != nil {
				msgID := data.AssistantMessageID
				if msgID == "" {
					msgID = "v2-step"
				}
				state.usageByMsg[msgID] = opencodeTokensToUsage(data.Tokens)
				state.usage = accumulateUsage(state.usageByMsg)
			}
			state.pendingStepSeparator = true
		}
		return true
	})

	if err != nil {
		return err
	}
	if streamErr != nil {
		return streamErr
	}
	if !sawDone {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// opencodeV2ExecutionError is a 2.x execution that failed or was
// interrupted.
type opencodeV2ExecutionError struct {
	event string
	err   *opencodeV2Error
}

func (e *opencodeV2ExecutionError) Error() string {
	msg := e.err.Message
	if msg == "" {
		msg = "no detail reported"
	}
	return "opencode " + e.event + ": " + e.err.Type + ": " + msg
}

func (e *opencodeV2ExecutionError) Unwrap() error { return nil }

// refused reports whether the server ended the turn on a provider content
// policy. opencode names the cause in the event's error type
// (provider.content-filter) and repeats it in the message, so both are read.
func (e *opencodeV2ExecutionError) refused() bool {
	return e != nil && e.err != nil && (refusesTurn(e.err.Type) || refusesTurn(e.err.Message))
}

// beginV2Text starts a text block for a 2.x assistant message.
func (s *opencodeStreamState) beginV2Text(messageID string, ordinal int) {
	if messageID == "" {
		return
	}
	if s.assistantMsgIDs == nil {
		s.assistantMsgIDs = make(map[string]bool)
	}
	s.assistantMsgIDs[messageID] = true
	partID := v2TextPartID(messageID, ordinal)
	part := s.textParts[partID]
	if part == nil {
		part = &opencodeTextPart{}
		s.textParts[partID] = part
		s.trackTextPart(partID)
	}
	part.messageID = messageID
}

// appendV2Text adds a streamed delta to a text block and emits it.
func (s *opencodeStreamState) appendV2Text(messageID string, ordinal int, delta string) {
	if messageID == "" || delta == "" {
		return
	}
	s.markV2Assistant(messageID)
	partID := v2TextPartID(messageID, ordinal)
	part := s.textParts[partID]
	if part == nil {
		part = &opencodeTextPart{}
		s.textParts[partID] = part
		s.trackTextPart(partID)
	}
	part.messageID = messageID
	part.text += delta
	s.emitTextPartChunk(part, partID)
}

// endV2Text closes a text block with its final text.
func (s *opencodeStreamState) endV2Text(messageID string, ordinal int, text string) {
	if messageID == "" {
		return
	}
	s.markV2Assistant(messageID)
	partID := v2TextPartID(messageID, ordinal)
	part := s.textParts[partID]
	if part == nil {
		part = &opencodeTextPart{}
		s.textParts[partID] = part
		s.trackTextPart(partID)
	}
	part.messageID = messageID
	if text != "" {
		part.text = text
	}
	s.emitTextPartChunk(part, partID)
}

// markV2Assistant records a message as the assistant's, so its text is
// emitted. A delta can arrive without a preceding session.text.started, so
// the marker is set wherever the message is first seen.
func (s *opencodeStreamState) markV2Assistant(messageID string) {
	if s.assistantMsgIDs == nil {
		s.assistantMsgIDs = make(map[string]bool)
	}
	s.assistantMsgIDs[messageID] = true
}

// v2TextPartID names the text block of one message and ordinal. The 2.x
// stream identifies a text block by the pair, where 1.x used a part id.
func v2TextPartID(messageID string, ordinal int) string {
	return messageID + "#" + strconv.Itoa(ordinal)
}
