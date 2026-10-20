package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string, env []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", agentSetupFailure(fmt.Errorf("opencode port: %w", err))
	}
	args := buildOpencodeServeArgs(a.extraArgs, port)
	// The credential is read from the SAME environment the server is
	// started with, so the server and the client always agree on it.
	auth := resolveOpencodeAuth(gitSafeEnvWithOverlay(cwd, a.overlay(), env))
	// A 2.x server generates a random password at startup unless one is
	// configured, and prints it once on stdout. The client watches the
	// output for that line, so the two always agree on the credential.
	capture, sink := newOpencodePasswordCapture()
	srv, err := startServerWithPortOutput(ctx, "opencode", a.bin, args, cwd, "/global/health", port, a.overlay(), sink, env)
	if err != nil {
		return "", agentSetupFailure(fmt.Errorf("opencode server: %w", err))
	}
	a.server = srv
	if auth.password == "" {
		// Nothing was configured, so the server generated one and printed
		// it. Wait briefly for that line: it is printed as the server
		// comes up, and the health check above already waited for it.
		auth = opencodeAuth{username: opencodeAuthUsername, password: capture.Wait(ctx, opencodePasswordWait)}
	}
	a.apiAuth = auth

	// The API generation decides every route this adapter uses, so it is
	// detected as soon as the server is up, before the first session is
	// created. A 2.x server answers the 1.x session route with 405, which
	// reads as a broken model rather than a version mismatch; detecting the
	// version is what turns that into a working request.
	version, err := detectOpencodeAPIVersion(ctx, srv.baseURL(), a.apiAuth)
	if err != nil {
		srv.shutdown()
		a.server = nil
		return "", agentSetupFailure(fmt.Errorf("opencode api version: %w", err))
	}
	a.apiVersion = version
	slog.Info("opencode api detected", "version", version.String())
	return srv.baseURL(), nil
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// extras inserted after the "serve" subcommand and before the managed flags.
func buildOpencodeServeArgs(extraArgs []string, port int) []string {
	args := make([]string, 0, len(extraArgs)+6)
	args = append(args, "serve")
	args = append(args, extraArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	return args
}

// The session, message, event-stream and teardown calls live in
// opencode_api.go: they are the parts of the adapter that differ between the
// OpenCode 1.x and 2.x API generations, and keeping them together makes the
// two shapes readable side by side.

// buildOpencodePrompt appends schema instructions to the prompt.
func buildOpencodePrompt(prompt string, schema json.RawMessage) string {
	return strings.Join([]string{
		prompt,
		"",
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}
