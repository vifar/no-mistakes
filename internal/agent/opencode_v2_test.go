package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// opencodeV2Fake is a fake OpenCode 2.x server: the /api routes, the
// session-create body shape, the non-blocking prompt endpoint, the 2.x event
// frames and the message list.
type opencodeV2Fake struct {
	server *httptest.Server

	sessions   atomic.Int32
	prompts    atomic.Int32
	messages   atomic.Int32
	deletes    atomic.Int32
	interrupts atomic.Int32

	// lastCreateBody records the body the adapter sent to create a session.
	lastCreateBody atomic.Value
	// lastPromptBody records the body the adapter sent with the prompt.
	lastPromptBody atomic.Value
	// lastAuthorization records the Authorization header of the last API call.
	lastAuthorization atomic.Value
}

// opencodeV2FakeOptions tunes the fake's answers.
type opencodeV2FakeOptions struct {
	// answer is the assistant text the turn produces.
	answer string
	// assistantTokens is the usage the step reports.
	assistantTokens string
	// toolRan makes the stream report a tool part.
	toolRan bool
	// execError, when set, makes the execution fail with this error.
	execError *opencodeV2Error
	// sessionCreateStatus, when non-zero, is the status the session-create
	// route answers with.
	sessionCreateStatus int
}

// newOpencodeV2Fake starts a fake 2.x server.
func newOpencodeV2Fake(t *testing.T, opts opencodeV2FakeOptions) *opencodeV2Fake {
	t.Helper()
	fake := &opencodeV2Fake{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/info" && r.Method == http.MethodGet:
			// The 2.x version probe answers JSON.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"2.0.32","pid":1,"urls":["http://127.0.0.1:1"]}`)

		case r.URL.Path == "/api/session" && r.Method == http.MethodPost:
			if opts.sessionCreateStatus != 0 {
				w.WriteHeader(opts.sessionCreateStatus)
				return
			}
			body, _ := readAllBody(r)
			fake.lastCreateBody.Store(string(body))
			fake.lastAuthorization.Store(r.Header.Get("Authorization"))
			id := fmt.Sprintf("v2s%d", fake.sessions.Add(1))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"id":%q,"projectID":"p1","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1,"updated":1},"location":{"directory":"/tmp"}}}`, id)

		case r.URL.Path == "/api/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			writeV2Event(w, `{"type":"server.connected","data":{}}`)
			if opts.toolRan {
				writeV2Event(w, `{"type":"session.tool.called","data":{"sessionID":"v2s1","assistantMessageID":"m1","id":"call1","name":"shell"}}`)
			}
			writeV2Event(w, `{"type":"session.text.started","data":{"sessionID":"v2s1","assistantMessageID":"m1","ordinal":0}}`)
			writeV2Event(w, fmt.Sprintf(`{"type":"session.text.delta","data":{"sessionID":"v2s1","assistantMessageID":"m1","ordinal":0,"delta":%q}}`, opts.answer))
			writeV2Event(w, fmt.Sprintf(`{"type":"session.text.ended","data":{"sessionID":"v2s1","assistantMessageID":"m1","ordinal":0,"text":%q}}`, opts.answer))
			if opts.assistantTokens != "" {
				writeV2Event(w, fmt.Sprintf(`{"type":"session.step.ended","data":{"sessionID":"v2s1","assistantMessageID":"m1","finish":"stop","tokens":%s}}`, opts.assistantTokens))
			}
			if opts.execError != nil {
				errJSON, _ := json.Marshal(opts.execError)
				writeV2Event(w, fmt.Sprintf(`{"type":"session.execution.failed","data":{"sessionID":"v2s1","error":%s}}`, errJSON))
			} else {
				writeV2Event(w, `{"type":"session.execution.succeeded","data":{"sessionID":"v2s1"}}`)
			}
			if flusher != nil {
				flusher.Flush()
			}

		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/prompt") && r.Method == http.MethodPost:
			body, _ := readAllBody(r)
			fake.lastPromptBody.Store(string(body))
			fake.prompts.Add(1)
			// The 2.x prompt endpoint does NOT block: it admits the prompt.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"id":"msg1","sessionID":"v2s1","time":{"created":1},"type":"user","payload":{"text":"hi"},"delivery":"steer"}}`)

		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/message") && r.Method == http.MethodGet:
			fake.messages.Add(1)
			tokens := opts.assistantTokens
			if tokens == "" {
				tokens = `{"input":100,"output":50,"reasoning":0,"cache":{"read":0,"write":0}}`
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":"m1","time":{"created":1},"type":"user","text":"hi"},{"id":"m2","time":{"created":2,"completed":3},"type":"assistant","agent":"build","model":{"id":"minimax-m3.1-flash","providerID":"ollama-cloud"},"content":[{"type":"text","text":%q}],"finish":"stop","tokens":%s}],"cursor":{}}`, opts.answer, tokens)

		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/interrupt"):
			fake.interrupts.Add(1)
			w.WriteHeader(http.StatusOK)

		case strings.HasPrefix(r.URL.Path, "/api/session/") && r.Method == http.MethodDelete:
			fake.deletes.Add(1)
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// writeV2Event writes one 2.x SSE frame.
func writeV2Event(w http.ResponseWriter, frame string) {
	fmt.Fprintf(w, "data: %s\n\n", frame)
}

// readAllBody reads a request body.
func readAllBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return string(buf), nil
		}
	}
}

// opencodeV2AgentFor returns an adapter pointed at the fake, with the API
// version already detected.
func opencodeV2AgentFor(fake *opencodeV2Fake) *opencodeAgent {
	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(fake.server.URL)},
	}
	a.setAPI(opencodeAPIVersion2, opencodeAuth{})
	return a
}

// TestOpencodeAgent_V2FullFlow is the regression for issue #112 on the 2.x
// side: the adapter must talk to the /api routes, create the session with
// the 2.x body, send the prompt to the 2.x endpoint and read the answer.
func TestOpencodeAgent_V2FullFlow(t *testing.T) {
	fake := newOpencodeV2Fake(t, opencodeV2FakeOptions{
		answer:          `{"summary":"all good"}`,
		assistantTokens: `{"input":100,"output":50,"reasoning":0,"cache":{"read":5,"write":2}}`,
	})
	a := opencodeV2AgentFor(fake)

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["summary"] != "all good" {
		t.Errorf("expected summary=all good, got %v", output["summary"])
	}
	// The adapter's convention counts the cache tokens inside the input
	// figure, exactly like the 1.x path: 100 input + 5 cache read + 2
	// cache write.
	if result.Usage.InputTokens != 107 || result.Usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want 107/50", result.Usage)
	}
	if result.Usage.CacheReadTokens != 5 || result.Usage.CacheCreationTokens != 2 {
		t.Errorf("cache usage = %+v, want 5/2", result.Usage)
	}
	if len(chunks) == 0 {
		t.Error("expected at least one streamed chunk")
	}
	if got := fake.sessions.Load(); got != 1 {
		t.Errorf("sessions = %d, want 1", got)
	}
	if got := fake.prompts.Load(); got != 1 {
		t.Errorf("prompts = %d, want 1", got)
	}
	if got := fake.deletes.Load(); got != 1 {
		t.Errorf("deletes = %d, want the session to be cleaned up", got)
	}
}

// TestOpencodeAgent_V2SessionCreateUsesTheV2Body pins the body shape: 2.x
// rejects the 1.x body outright, so `directory` and `permission` must not
// be sent.
func TestOpencodeAgent_V2SessionCreateUsesTheV2Body(t *testing.T) {
	fake := newOpencodeV2Fake(t, opencodeV2FakeOptions{answer: "ok"})
	a := opencodeV2AgentFor(fake)
	a.profile = agentcfg.Profile{Model: "ollama-cloud/minimax-m3.1-flash"}

	cwd := t.TempDir()
	if _, err := a.createSession(context.Background(), fake.server.URL, cwd); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	body, _ := fake.lastCreateBody.Load().(string)
	if body == "" {
		t.Fatal("no session-create body recorded")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("session-create body is not JSON: %v (%s)", err, body)
	}
	if _, ok := decoded["directory"]; ok {
		t.Errorf("session-create body carries the 1.x `directory` key: %s", body)
	}
	if _, ok := decoded["permission"]; ok {
		t.Errorf("session-create body carries the 1.x `permission` key: %s", body)
	}
	location, ok := decoded["location"].(map[string]any)
	if !ok {
		t.Fatalf("session-create body has no `location`: %s", body)
	}
	if location["directory"] != cwd {
		t.Errorf("location.directory = %v, want %q", location["directory"], cwd)
	}
	permissions, ok := decoded["permissions"].([]any)
	if !ok || len(permissions) == 0 {
		t.Fatalf("session-create body has no `permissions`: %s", body)
	}
	rule, ok := permissions[0].(map[string]any)
	if !ok {
		t.Fatalf("permission rule is not an object: %s", body)
	}
	if rule["action"] != "*" || rule["resource"] != "*" || rule["effect"] != "allow" {
		t.Errorf("permission rule = %v, want the 2.x {action,resource,effect} shape", rule)
	}
	model, ok := decoded["model"].(map[string]any)
	if !ok {
		t.Fatalf("session-create body has no `model`: %s", body)
	}
	// 2.x names the model {id, providerID}; the 1.x {providerID, modelID}
	// shape is rejected by the server.
	if model["providerID"] != "ollama-cloud" || model["id"] != "minimax-m3.1-flash" {
		t.Errorf("model = %v, want the requested provider/model", model)
	}
	if _, ok := model["modelID"]; ok {
		t.Errorf("model carries the 1.x `modelID` key: %s", body)
	}
}

// TestOpencodeAgent_V2PromptUsesTheV2Endpoint pins the prompt body: 2.x
// takes a plain `text` field, not the 1.x `parts` array.
func TestOpencodeAgent_V2PromptUsesTheV2Endpoint(t *testing.T) {
	fake := newOpencodeV2Fake(t, opencodeV2FakeOptions{answer: "ok"})
	a := opencodeV2AgentFor(fake)

	if _, err := a.sendMessage(context.Background(), fake.server.URL, "v2s1", "hello", nil); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	body, _ := fake.lastPromptBody.Load().(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("prompt body is not JSON: %v (%s)", err, body)
	}
	if decoded["text"] != "hello" {
		t.Errorf("prompt body text = %v, want hello", decoded["text"])
	}
	if _, ok := decoded["parts"]; ok {
		t.Errorf("prompt body carries the 1.x `parts` key: %s", body)
	}
}

// TestOpencodeAgent_V2DetectsTheAPIVersion pins the version probe: a server
// that answers /api/info with JSON is 2.x, and one that answers the
// single-page app's HTML is 1.x.
func TestOpencodeAgent_V2DetectsTheAPIVersion(t *testing.T) {
	t.Run("2.x answers the probe with json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"2.0.32"}`)
		}))
		defer server.Close()

		version, err := detectOpencodeAPIVersion(context.Background(), server.URL, opencodeAuth{})
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if version != opencodeAPIVersion2 {
			t.Fatalf("version = %v, want 2.x", version)
		}
	})

	t.Run("1.x answers the probe with the single page app", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><html></html>")
		}))
		defer server.Close()

		version, err := detectOpencodeAPIVersion(context.Background(), server.URL, opencodeAuth{})
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if version != opencodeAPIVersion1 {
			t.Fatalf("version = %v, want 1.x", version)
		}
	})

	t.Run("an unreachable server is not a version answer", func(t *testing.T) {
		version, err := detectOpencodeAPIVersion(context.Background(), "http://127.0.0.1:1", opencodeAuth{})
		if err == nil {
			t.Fatal("detect error = nil, want the probe's own failure")
		}
		if version != opencodeAPIVersionUnknown {
			t.Fatalf("version = %v, want unknown", version)
		}
	})
}

// TestOpencodeAgent_V2SendsBasicAuth proves the 2.x credential reaches the
// server: 2.x answers every /api request with 401 unless the basic
// credential is right.
func TestOpencodeAgent_V2SendsBasicAuth(t *testing.T) {
	const password = "s3cret-opencode-password"
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"_tag":"UnauthorizedError","message":"Authentication required"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"version":"2.0.32"}`)
	}))
	defer server.Close()

	auth := opencodeAuth{username: "opencode", password: password}
	version, err := detectOpencodeAPIVersion(context.Background(), server.URL, auth)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if version != opencodeAPIVersion2 {
		t.Fatalf("version = %v, want 2.x", version)
	}
	header, _ := seen.Load().(string)
	if !strings.HasPrefix(header, "Basic ") {
		t.Fatalf("Authorization = %q, want a basic credential", header)
	}
}

// TestOpencodeAgent_V2SessionCreateFailureIsASetupFailure keeps issue
// #112's rule on the 2.x path: a rejected session create is an agent that
// is unavailable, so the fallback wrapper moves on.
func TestOpencodeAgent_V2SessionCreateFailureIsASetupFailure(t *testing.T) {
	fake := newOpencodeV2Fake(t, opencodeV2FakeOptions{sessionCreateStatus: http.StatusUnauthorized})
	a := opencodeV2AgentFor(fake)

	_, err := a.createSession(context.Background(), fake.server.URL, t.TempDir())
	if err == nil {
		t.Fatal("createSession error = nil, want the failure")
	}
	if !isAgentUnavailableError(err) {
		t.Fatalf("error = %v, want an agent-unavailable failure", err)
	}
}

// TestOpencodeAgent_V2ExecutionFailureSurfacesTheServerReason keeps the
// server's own reason visible: a failed turn must not read as a missing
// answer.
func TestOpencodeAgent_V2ExecutionFailureSurfacesTheServerReason(t *testing.T) {
	fake := newOpencodeV2Fake(t, opencodeV2FakeOptions{
		answer: "partial",
		execError: &opencodeV2Error{
			Type:    "provider.no-route",
			Message: "Model unavailable: ollama-cloud/no-such-model",
		},
	})
	a := opencodeV2AgentFor(fake)

	_, err := a.Run(context.Background(), RunOpts{
		Prompt: "review this code",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("Run error = nil, want the execution failure")
	}
	if !strings.Contains(err.Error(), "Model unavailable") {
		t.Fatalf("error = %v, want the server's own reason", err)
	}
}

// TestOpencodeAgent_V2ToolActivityIsRecorded proves the replay gate can see
// a tool: the 2.x stream names tool activity explicitly.
func TestOpencodeAgent_V2ToolActivityIsRecorded(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"type\":\"session.tool.called\",\"data\":{\"sessionID\":\"v2s1\",\"assistantMessageID\":\"m1\",\"id\":\"call1\",\"name\":\"shell\"}}\n\n" +
			"data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"v2s1\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"delta\":\"done\"}}\n\n" +
			"data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"v2s1\"}}\n\n")
	state := &opencodeStreamState{
		sessionID:  "v2s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	if err := parseOpencodeV2SSE(stream, state); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !state.toolInvoked {
		t.Fatal("toolInvoked = false, want the tool part to be recorded")
	}
	if evidence := state.v2ToolEvidence(); evidence != opencodeToolsRan {
		t.Fatalf("evidence = %v, want ran tools", evidence)
	}
	if state.lastText != "done" {
		t.Fatalf("lastText = %q, want done", state.lastText)
	}
}

// TestOpencodeAgent_V2OtherSessionsAreIgnored proves the server-wide event
// stream cannot leak another session's turn into this one.
func TestOpencodeAgent_V2OtherSessionsAreIgnored(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"other\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"delta\":\"not mine\"}}\n\n" +
			"data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"v2s1\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"delta\":\"mine\"}}\n\n" +
			"data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"v2s1\"}}\n\n")
	state := &opencodeStreamState{
		sessionID:  "v2s1",
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	if err := parseOpencodeV2SSE(stream, state); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if state.lastText != "mine" {
		t.Fatalf("lastText = %q, want mine", state.lastText)
	}
}
