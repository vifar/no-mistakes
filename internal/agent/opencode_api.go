package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// basicAuthValue renders the value of an HTTP basic Authorization header.
func basicAuthValue(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// opencodeAPIVersion is the wire generation of the OpenCode server API this
// adapter speaks.
//
// OpenCode 1.x served its API at the root: POST /session, POST
// /session/{id}/message, GET /global/event. OpenCode 2.x moved the whole
// surface under /api and changed the shapes: POST /api/session (with
// `location` and `permissions` instead of `directory` and `permission`),
// POST /api/session/{id}/prompt (which does not block on the turn), and
// GET /api/event (whose frames carry a top-level `type` instead of
// `payload.type`).
//
// The 2.x server ALSO answers the 1.x paths with the single-page app, so
// POST /session returns 405 on 2.x and the failure looks like a broken
// model rather than a version mismatch. That is exactly what issue #112
// measured.
type opencodeAPIVersion int

const (
	// opencodeAPIVersionUnknown is the zero value: the version has not been
	// detected yet.
	opencodeAPIVersionUnknown opencodeAPIVersion = iota
	// opencodeAPIVersion1 is the 1.x root-level API.
	opencodeAPIVersion1
	// opencodeAPIVersion2 is the 2.x /api-prefixed API.
	opencodeAPIVersion2
)

func (v opencodeAPIVersion) String() string {
	switch v {
	case opencodeAPIVersion1:
		return "1.x"
	case opencodeAPIVersion2:
		return "2.x"
	default:
		return "unknown"
	}
}

// opencodeAuth is the credential the 2.x server requires on every /api
// request.
//
// OpenCode 2.x protects its API with HTTP basic auth. The username is
// fixed ("opencode") and the password is the server's own, which it reads
// from OPENCODE_PASSWORD or OPENCODE_SERVER_PASSWORD and otherwise generates
// at startup. The 1.x server had no such protection unless
// OPENCODE_SERVER_PASSWORD was set.
//
// The adapter and server must share a credential: an independent client
// password would only produce a misleading 401. ensureServer resolves the
// effective child environment and captures a generated startup password
// when the server chooses its own credential. Only an
// unsecured 1.x server needs no credential. Operator-facing environment semantics are
// owned by docs/src/content/docs/reference/environment.md.
type opencodeAuth struct {
	username string
	password string
}

// opencodeAuthUsername is the fixed basic-auth username OpenCode expects.
// Measured: an empty username is rejected with 401, so it is not optional.
const opencodeAuthUsername = "opencode"

// opencodePasswordEnvVars are the environment variables OpenCode reads the
// server password from, in the order it prefers them.
var opencodePasswordEnvVars = []string{"OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD"}

// resolveOpencodeAuth reads the server password out of an environment.
//
// The environment is the one the agent server process was started with, so
// the server and the client read the same value. An environment without a
// password yields an empty credential; ensureServer then attempts to capture
// a generated password from startup output before making API requests.
func resolveOpencodeAuth(env []string) opencodeAuth {
	for _, name := range opencodePasswordEnvVars {
		if value, ok := lookupEnvValue(env, name); ok && value != "" {
			return opencodeAuth{username: opencodeAuthUsername, password: value}
		}
	}
	return opencodeAuth{}
}

// lookupEnvValue reads a variable out of an environment slice.
func lookupEnvValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		entry := env[i]
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// opencodeAuthHeader returns the Authorization header value, or "" when no
// password is configured.
func (a opencodeAuth) header() string {
	if a.password == "" {
		return ""
	}
	return "Basic " + basicAuthValue(a.username, a.password)
}

// opencodeAPIRoutes holds the path fragments for one API generation, so the
// request builders below never spell a version's paths inline.
type opencodeAPIRoutes struct {
	sessionCreate    string
	sessionDelete    string
	sessionPrompt    string
	sessionInterrupt string
	eventStream      string
}

// opencodeRoutes1 is the 1.x root-level API.
func opencodeRoutes1() opencodeAPIRoutes {
	return opencodeAPIRoutes{
		sessionCreate:    "/session",
		sessionDelete:    "/session/%s",
		sessionPrompt:    "/session/%s/message",
		sessionInterrupt: "/session/%s/abort",
		eventStream:      "/global/event",
	}
}

// opencodeRoutes2 is the 2.x /api-prefixed API.
func opencodeRoutes2() opencodeAPIRoutes {
	return opencodeAPIRoutes{
		sessionCreate:    "/api/session",
		sessionDelete:    "/api/session/%s",
		sessionPrompt:    "/api/session/%s/prompt",
		sessionInterrupt: "/api/session/%s/interrupt",
		eventStream:      "/api/event",
	}
}

// opencodeRoutes returns the routes for a version.
func opencodeRoutes(version opencodeAPIVersion) opencodeAPIRoutes {
	if version == opencodeAPIVersion2 {
		return opencodeRoutes2()
	}
	return opencodeRoutes1()
}

// detectOpencodeAPIVersion asks the server which API generation it serves.
//
// The probe is GET /api/info: on 2.x it answers JSON carrying a version, and
// on 1.x the route does not exist and the server answers the single-page
// app's HTML. A JSON content type therefore identifies 2.x, and anything
// else identifies 1.x.
//
// The probe is answered DIFFERENTLY by the two generations when the server
// is secured, and the difference has to be read carefully:
//
//   - A 2.x server answers 401 with a JSON body ({"_tag":"UnauthorizedError"}).
//   - A 1.x server answers 401 with an EMPTY body and a Basic challenge.
//
// Both are "the request was not authorized", but only one of them is a 2.x
// server. A JSON 401 therefore identifies 2.x as firmly as a JSON 200 does,
// because the 2.x error envelope is the only one of the two that carries a
// body. A 1.x 401 (empty body) is a 1.x server that simply wants the
// credential.
//
// A probe that cannot be completed at all (the server is not up, the request
// timed out) is NOT a version answer: it reports the unknown version so the
// caller fails with the probe's own error instead of guessing.
func detectOpencodeAPIVersion(ctx context.Context, baseURL string, auth opencodeAuth) (opencodeAPIVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/info", nil)
	if err != nil {
		return opencodeAPIVersionUnknown, fmt.Errorf("opencode version probe: %w", err)
	}
	if header := auth.header(); header != "" {
		req.Header.Set("Authorization", header)
	}
	resp, err := opencodeV2HTTP.Do(req)
	if err != nil {
		return opencodeAPIVersionUnknown, fmt.Errorf("opencode version probe: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// The content type decides it. A 2.x server answers its own routes
	// with JSON, including its 401 error envelope; a 1.x server answers
	// its unknown routes with the single-page app's HTML, and its 401
	// with no body at all.
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		return opencodeAPIVersion2, nil
	}
	// A 2.x server that rejected the credential still answered with its
	// JSON envelope, but a server may omit the content type on an error.
	// The body is the second witness.
	if resp.StatusCode == http.StatusUnauthorized && strings.Contains(string(body), `"_tag"`) {
		return opencodeAPIVersion2, nil
	}
	return opencodeAPIVersion1, nil
}

// opencodeSessionCreateResponse is the 2.x session-create answer. The 2.x
// API wraps every resource in a `data` envelope; 1.x returns the session
// object at the top level.
type opencodeSessionCreateResponse struct {
	Data *opencodeSessionInfo `json:"data,omitempty"`
}

// opencodeSessionInfo is the session identity both generations report.
type opencodeSessionInfo struct {
	ID string `json:"id"`
}

// opencodeCreateSession creates a session and returns its id.
//
// The body differs by generation. 1.x takes `directory` and a `permission`
// ruleset; 2.x takes `location: {directory}` and a `permissions` ruleset
// whose entries are {action, resource, effect}. 2.x REJECTS the 1.x body
// (additionalProperties is false), so the two cannot share one shape.
func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	if a.version() == opencodeAPIVersion2 && a.profile.Effort != "" {
		if _, _, ok := agentcfg.SplitProviderModel(a.profile.Model); !ok {
			return "", errors.New("opencode 2.x requires an explicit provider/model to apply reasoning effort")
		}
	}
	body := a.sessionCreateBody(cwd)
	resp, err := a.doJSON(ctx, http.MethodPost, baseURL+a.routes().sessionCreate, body)
	if err != nil {
		// The session never existed, so opencode never held the prompt and
		// no tool could have run. That makes the agent unavailable rather
		// than wrong, and the fallback wrapper moves to the next agent
		// instead of failing the run (issue #112).
		return "", agentSetupFailure(fmt.Errorf("opencode create session: %w", err))
	}

	id, err := a.sessionIDFromCreate(resp)
	if err != nil {
		return "", agentSetupFailure(err)
	}
	return id, nil
}

// sessionCreateBody builds the session-create body for the detected version.
func (a *opencodeAgent) sessionCreateBody(cwd string) map[string]any {
	body := map[string]any{}
	if a.version() == opencodeAPIVersion2 {
		body["location"] = map[string]any{"directory": cwd}
		body["permissions"] = []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
		}
	} else {
		body["directory"] = cwd
		body["permission"] = []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		}
	}
	// The model and the reasoning effort ride the session body on 2.x,
	// which has no launch flag for either. 1.x carried them on the
	// message body instead (see messageBody).
	//
	// The two generations name the model differently: 1.x uses
	// {providerID, modelID}, and 2.x uses {id, providerID, variant}. The
	// server rejects the other generation's shape outright.
	if a.version() == opencodeAPIVersion2 {
		if provider, modelID, ok := agentcfg.SplitProviderModel(a.profile.Model); ok {
			model := map[string]string{"id": modelID, "providerID": provider}
			if a.profile.Effort != "" {
				model["variant"] = string(a.profile.Effort)
			}
			body["model"] = model
		}
	}
	return body
}

// sessionIDFromCreate reads the session id out of a create answer, from
// either the 2.x `data` envelope or the 1.x top-level object.
func (a *opencodeAgent) sessionIDFromCreate(resp []byte) (string, error) {
	if a.version() == opencodeAPIVersion2 {
		var wrapped opencodeSessionCreateResponse
		if err := json.Unmarshal(resp, &wrapped); err != nil {
			return "", agentSetupFailure(fmt.Errorf("opencode create session parse: %w", err))
		}
		if wrapped.Data == nil || wrapped.Data.ID == "" {
			return "", agentSetupFailure(errors.New("opencode create session: the server returned no session id"))
		}
		return wrapped.Data.ID, nil
	}
	var result opencodeSessionInfo
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", agentSetupFailure(fmt.Errorf("opencode create session parse: %w", err))
	}
	if result.ID == "" {
		return "", agentSetupFailure(errors.New("opencode create session: the server returned no session id"))
	}
	return result.ID, nil
}

// messageBody builds the 1.x prompt payload.
//
// 1.x carries the model, reasoning effort, and optional native output format
// in the message body. The 2.x payload is built separately in runOnceV2;
// runOnceV2 owns its prompt-only structured-output path.
func (a *opencodeAgent) messageBody(prompt string, schema json.RawMessage) map[string]any {
	body := map[string]any{
		"role":  "user",
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := agentcfg.SplitProviderModel(a.profile.Model); ok {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if a.profile.Effort != "" {
		body["variant"] = string(a.profile.Effort)
	}
	if len(schema) > 0 {
		body["info"] = map[string]any{
			"format": map[string]any{
				"type":       "json_schema",
				"schema":     json.RawMessage(schema),
				"retryCount": 2,
			},
		}
	}
	return body
}

// sendMessage submits a 1.x prompt and waits for its synchronous response.
func (a *opencodeAgent) sendMessage(ctx context.Context, baseURL, sessionID, prompt string, schema json.RawMessage) (*opencodeMessageResponse, error) {
	respBytes, err := a.doJSON(ctx, http.MethodPost, baseURL+fmt.Sprintf(a.routes().sessionPrompt, sessionID), a.messageBody(prompt, schema))
	if err != nil {
		return nil, err
	}

	var resp opencodeMessageResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("opencode message parse: %w", err)
	}
	return &resp, nil
}

// opencodeV2Message is one projected message of a 2.x session. The
// assistant message is the one whose type is "assistant".
type opencodeV2Message struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Role    string           `json:"role"`
	Text    string           `json:"text"`
	Content []opencodeV2Part `json:"content"`
	Finish  string           `json:"finish"`
	Tokens  *opencodeTokens  `json:"tokens"`
	Error   *opencodeV2Error `json:"error"`
	Outcome string           `json:"outcome"`
}

// opencodeV2Part is one content block of a 2.x assistant message.
type opencodeV2Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// opencodeV2Error is the structured error a 2.x message carries.
type opencodeV2Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// connectEventStream opens the server-sent event stream.
func (a *opencodeAgent) connectEventStream(ctx context.Context, baseURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+a.routes().eventStream, nil)
	if err != nil {
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream request: %w", err))
	}
	req.Header.Set("Accept", "text/event-stream")
	if header := a.auth().header(); header != "" {
		req.Header.Set("Authorization", header)
	}

	resp, err := opencodeV2HTTP.Do(req)
	if err != nil {
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, agentSetupFailure(fmt.Errorf("opencode event stream failed with %d: %s", resp.StatusCode, string(body)))
	}

	return resp.Body, nil
}

// abortSession stops the running turn.
func (a *opencodeAgent) abortSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a.doJSON(ctx, http.MethodPost, baseURL+fmt.Sprintf(a.routes().sessionInterrupt, sessionID), nil)
}

// deleteSession removes the session.
func (a *opencodeAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+fmt.Sprintf(a.routes().sessionDelete, sessionID), nil)
	if err != nil {
		return
	}
	if header := a.auth().header(); header != "" {
		req.Header.Set("Authorization", header)
	}
	resp, err := opencodeV2HTTP.Do(req)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
}

// doJSON makes an authenticated JSON request and returns the response body.
func (a *opencodeAgent) doJSON(ctx context.Context, method, url string, body any) ([]byte, error) {
	return (opencodeV2Client{authorization: a.auth().header()}).raw(ctx, method, url, body)
}

// routes returns the routes of the detected API generation.
func (a *opencodeAgent) routes() opencodeAPIRoutes {
	return opencodeRoutes(a.version())
}

// version returns the detected API generation, defaulting to 1.x so an
// undetected server keeps the historical behaviour.
func (a *opencodeAgent) version() opencodeAPIVersion {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.apiVersion == opencodeAPIVersionUnknown {
		return opencodeAPIVersion1
	}
	return a.apiVersion
}

// auth returns the credential for the 2.x server.
func (a *opencodeAgent) auth() opencodeAuth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apiAuth
}

// setAPI records the detected API generation and credential.
func (a *opencodeAgent) setAPI(version opencodeAPIVersion, auth opencodeAuth) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.apiVersion = version
	a.apiAuth = auth
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
