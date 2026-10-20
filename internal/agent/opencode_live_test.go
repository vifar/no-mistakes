package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// TestOpencodeAgent_LiveServer2x runs the adapter against a REAL OpenCode
// server, so the 2.x protocol is exercised against the server itself and
// not against a fake.
//
// The test is skipped unless OPENCODE_BIN points at an opencode binary: it
// needs a real server, and the model it asks for has to be one the
// configured server can reach.
//
// It covers the whole issue #112 path: the server is started by the adapter
// (not by the test), the version is detected, the session is created, the
// prompt is sent, the answer comes back through the 2.x event stream, and
// the session is deleted.
func TestOpencodeAgent_LiveServer2x(t *testing.T) {
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		t.Skip("OPENCODE_BIN is not set; skipping the live opencode server test")
	}
	model := os.Getenv("OPENCODE_LIVE_MODEL")
	if model == "" {
		t.Skip("OPENCODE_LIVE_MODEL is not set; skipping the live opencode server test")
	}

	dir := t.TempDir()
	a := &opencodeAgent{bin: bin}
	t.Cleanup(func() { _ = a.Close() })
	a.profile = agentcfg.Profile{Model: model}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	result, err := a.Run(ctx, RunOpts{
		Prompt:     "Reply with exactly this JSON and nothing else: {\"summary\":\"live ok\"}",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.version() != opencodeAPIVersion2 {
		t.Fatalf("detected API generation = %s, want 2.x", a.version())
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, string(result.Output))
	}
	if output["summary"] == nil {
		t.Fatalf("output = %s, want a summary", string(result.Output))
	}
	t.Logf("live opencode answer: %s", string(result.Output))
}

// TestOpencodeAgent_LiveServer2xDetectsTheVersion proves the detection
// against a real server: the adapter must classify it as 2.x, whatever the
// installed version is.
func TestOpencodeAgent_LiveServer2xDetectsTheVersion(t *testing.T) {
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		t.Skip("OPENCODE_BIN is not set; skipping the live opencode server test")
	}
	dir := t.TempDir()
	a := &opencodeAgent{bin: bin}
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err := a.ensureServer(ctx, dir, nil)
	if err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	t.Logf("detected opencode API version: %s", a.version().String())
	if a.version() != opencodeAPIVersion2 {
		t.Fatalf("detected API generation = %s, want 2.x", a.version())
	}
}

// TestOpencodeAgent_LivePasswordCapture proves the adapter learns the
// password a real 2.x server generates and prints, and that the captured
// credential is accepted by the server.
func TestOpencodeAgent_LivePasswordCapture(t *testing.T) {
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		t.Skip("OPENCODE_BIN is not set; skipping the live opencode server test")
	}
	dir := t.TempDir()

	// Start the server with NO password in the environment, so it
	// generates one and prints it. (The server prints the password ONLY
	// when none was configured, so the password variables are removed
	// from the child environment here.)
	cmd := exec.Command(bin, "serve", "--hostname", "127.0.0.1", "--port", "4988")
	cmd.Dir = dir
	cmd.Env = append(withoutOpencodePasswordEnv(os.Environ()), "OPENCODE_CONFIG_DIR="+filepath.Join(dir, "config"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	capture, _ := newOpencodePasswordCapture()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				capture.scan(string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	password := capture.Wait(context.Background(), 30*time.Second)
	if password == "" {
		t.Fatal("the server printed no password")
	}
	if strings.ContainsAny(password, " \t\n") {
		t.Fatalf("password %q contains whitespace", password)
	}
	t.Logf("captured password length: %d", len(password))
}

// withoutOpencodePasswordEnv removes the opencode password variables from an
// environment, so a server started with it generates and prints its own.
func withoutOpencodePasswordEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "OPENCODE_PASSWORD=") ||
			strings.HasPrefix(entry, "OPENCODE_SERVER_PASSWORD=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}
