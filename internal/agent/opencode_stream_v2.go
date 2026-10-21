package agent

import (
	"encoding/json"
	"fmt"
	"io"
)

type opencodeV2Stream struct {
	sessionID     string
	toolInvoked   bool
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
			if data.MessageID != "" && data.ToolID != "" {
				s.toolInvoked = true
			}
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
