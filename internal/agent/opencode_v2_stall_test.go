package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestOpencodeV2StallProgress(t *testing.T) {
	for _, mode := range []string{"tools", "text", "reasoning", "foreign", "missing_tool_identity", "silent"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			done := make(chan struct{})
			var doneOnce sync.Once
			var events atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "POST /api/session":
					fmt.Fprint(w, `{"data":{"id":"ses_fixture"}}`)
				case "GET /api/event":
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					select {
					case <-started:
					case <-r.Context().Done():
						return
					}
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()
					for i := 0; i < 15; i++ {
						select {
						case <-ticker.C:
						case <-r.Context().Done():
							return
						}
						kind := []string{"session.tool.called", "session.tool.success", "session.tool.failed"}[i%3]
						data := map[string]any{"sessionID": "ses_fixture", "assistantMessageID": "msg_final", "id": fmt.Sprintf("tool_%d", i), "input": map[string]string{"command": "pwd"}, "executed": false, "content": []any{map[string]string{"type": "text", "text": "result"}}, "error": map[string]string{"type": "tool.execution", "message": "failed"}}
						switch mode {
						case "reasoning":
							kind = "session.reasoning.delta"
							data["delta"] = "thinking"
						case "foreign":
							data["sessionID"] = "ses_other"
						case "missing_tool_identity":
							delete(data, "id")
						case "text":
							kind = "session.text.delta"
							data["delta"] = "text"
							data["ordinal"] = 0
						case "silent":
							continue
						}
						encoded, _ := json.Marshal(map[string]any{"type": kind, "data": data})
						fmt.Fprintf(w, "data: %s\n\n", encoded)
						w.(http.Flusher).Flush()
						events.Add(1)
					}
					doneOnce.Do(func() { close(done) })
				case "POST /api/session/ses_fixture/prompt":
					close(started)
					fmt.Fprint(w, `{}`)
				case "POST /api/experimental/session/ses_fixture/wait":
					select {
					case <-done:
						w.WriteHeader(http.StatusNoContent)
					case <-r.Context().Done():
					}
				case "GET /api/session/ses_fixture":
					fmt.Fprint(w, `{"data":{"id":"ses_fixture","outcome":"succeeded"}}`)
				case "GET /api/session/ses_fixture/message":
					fmt.Fprint(w, `{"data":[{"id":"msg_final","type":"assistant","content":[{"type":"text","text":"done"}]}],"cursor":{"next":null}}`)
				case "POST /api/session/ses_fixture/interrupt", "DELETE /api/session/ses_fixture":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sctx := &pipeline.StepContext{Ctx: ctx, Agent: agent.NewOpencodeV2TestAgent(server.URL), Config: &config.Config{AgentTimeout: time.Minute, AgentStallTimeout: 500 * time.Millisecond}, Log: func(string) {}}
			start := time.Now()
			result, err := sctx.RunAgent(agent.RunOpts{CWD: t.TempDir(), Prompt: "work"})
			if mode == "tools" || mode == "text" {
				if err != nil || result == nil || result.Text != "done" || events.Load() != 15 {
					t.Fatalf("progressing turn interrupted: result=%+v events=%d err=%v", result, events.Load(), err)
				}
				if time.Since(start) < 3*500*time.Millisecond {
					t.Fatal("turn did not outlive the stall budget")
				}
			} else {
				if !errors.Is(err, pipeline.ErrAgentStall) {
					t.Fatalf("progressless turn escaped stall bound: %v", err)
				}
				if mode != "silent" && events.Load() == 0 {
					t.Fatal("did not exercise rejected events")
				}
			}
		})
	}
}
