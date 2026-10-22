package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAntigravityInvocationEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell executable fixture")
	}
	t.Setenv("NM_AGY_INHERITED", "ambient")
	t.Setenv("NM_AGY_OVERRIDE", "ambient")
	t.Setenv("GIT_EDITOR", "interactive-editor")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "issue80.count")
	t.Setenv("GIT_CONFIG_VALUE_0", "count-kept")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'issue80.parameter=parameter-kept'")
	root := t.TempDir()
	bin := filepath.Join(root, "agy")
	script := `#!/bin/sh
set -eu
printf '{"event":"result","result":{"status":"SUCCESS","response":"%s"}}\n' "$NM_AGY_INHERITED|$NM_AGY_OVERLAY|$NM_AGY_OVERRIDE|${NM_AGY_REQUEST_ONLY-unset}|$GIT_EDITOR|$GIT_SEQUENCE_EDITOR|$GIT_TERMINAL_PROMPT|$NO_MISTAKES_GATE|$COMPACT_ADVISER_DISABLE|$(git config --get issue80.count)|$(git config --get issue80.parameter)"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ag, err := NewWithOptions(types.AgentAntigravity, bin, nil, Options{Environment: runenv.Overlay{Set: map[string]string{"NM_AGY_OVERLAY": "overlay", "NM_AGY_OVERRIDE": "overlay"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	for _, first := range []bool{true, false} {
		opts := RunOpts{CWD: root, Prompt: "probe"}
		expectedOverride := "overlay"
		expectedRequest := "unset"
		expectedEditor := "true"
		if first {
			opts.Env = []string{"NM_AGY_OVERRIDE=invocation", "NM_AGY_REQUEST_ONLY=this-request", "GIT_EDITOR=request-editor", "NO_MISTAKES_GATE=0", "COMPACT_ADVISER_DISABLE=0"}
			expectedOverride = "invocation"
			expectedRequest = "this-request"
			expectedEditor = "request-editor"
		}
		result, err := ag.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Join([]string{"ambient", "overlay", expectedOverride, expectedRequest, expectedEditor, "true", "0", "1", "1", "count-kept", "parameter-kept"}, "|")
		if result.Text != want {
			t.Fatalf("selected environment fields = %q, want %q", result.Text, want)
		}
	}
}
