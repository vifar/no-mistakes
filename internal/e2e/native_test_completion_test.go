//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The real daemon must use process completion, not an agent's early go
// payload, as its oracle. The scripted external agent emits the same payload
// in every mode; only its required child's execution and exit status differ.
func TestNativeTestCompletionOnRealBinary(t *testing.T) {
	for _, mode := range []string{"wait-success", "wait-failure", "early-exit", "accepted-submission", "rejected-submission"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			wrapper := filepath.Join(h.BinDir, "claude-completion-probe")
			payload := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"summary":"","tested":["child command"],"testing_summary":"child claimed success","artifacts":[],"scenarios":[{"name":"child command","result":"pass","live":true,"evidence":"scripted input","reason":""}],"verdict":"go"}}`
			if strings.HasSuffix(mode, "-submission") {
				var terminal map[string]any
				if err := json.Unmarshal([]byte(payload), &terminal); err != nil {
					t.Fatal(err)
				}
				structured := terminal["structured_output"]
				delete(terminal, "structured_output")
				terminal["result"] = "prose-only completion"
				events := []map[string]any{
					{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "submission-1", "name": "StructuredOutput", "input": structured}}}},
					{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "submission-1", "is_error": mode == "rejected-submission"}}}},
					terminal,
				}
				var lines []string
				for _, event := range events {
					encoded, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					lines = append(lines, string(encoded))
				}
				payload = strings.Join(lines, "\n")
			}
			completion := "sleep 0.05 &\nchild=$!\n"
			finish := "wait \"$child\"\nexit $?\n"
			if mode == "wait-failure" {
				completion = "(sleep 0.05; exit 7) &\nchild=$!\n"
			}
			if mode == "early-exit" {
				completion = "sleep 30 >/dev/null 2>&1 &\n"
				finish = "exit 0\n"
			}
			script := "#!/bin/sh\nprompt=$(mktemp)\ncat > \"$prompt\"\n" +
				"case \"$(cat \"$prompt\")\" in\n" +
				"  *'You are validating a code change by driving the product itself.'*|*'Your previous structured findings were REJECTED'*)\n" +
				"    rm -f \"$prompt\"\n" + completion +
				"printf '%s\\n' " + shellQuote(payload) + "\n" + finish +
				"    ;;\n" +
				"  *) " + shellQuote(filepath.Join(h.BinDir, "claude")) + " \"$@\" < \"$prompt\"; code=$?; rm -f \"$prompt\"; exit \"$code\" ;;\n" +
				"esac\n"
			if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(h.NMHome, "config.yaml")
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			config = []byte(strings.ReplaceAll(string(config), filepath.Join(h.BinDir, "claude"), wrapper))
			if err := os.WriteFile(configPath, config, 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/native-completion-" + mode
			h.CommitChange(branch, "feature.txt", "native child completion\n", "add completion probe")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 90*time.Second)
			step, ok := findStep(run.Steps, types.StepTest)
			if !ok {
				t.Fatalf("run has no test result: %+v", run)
			}
			if mode == "wait-success" || mode == "accepted-submission" {
				if run.Status != types.RunCompleted || step.Status != types.StepStatusCompleted {
					t.Fatalf("completed child was refused: run=%s error=%s test=%s", run.Status, deref(run.Error), step.Status)
				}
			} else {
				want := "exit status 7"
				if mode == "early-exit" {
					want = "native check completion is unverified"
				}
				if mode == "rejected-submission" {
					want = "claude returned no structured output"
				}
				if run.Status != types.RunFailed || step.Status != types.StepStatusFailed || step.Error == nil || !strings.Contains(*step.Error, want) {
					t.Fatalf("unsafe go payload was accepted: run=%s error=%s test=%s error=%s", run.Status, deref(run.Error), step.Status, deref(step.Error))
				}
			}
			log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "test.log"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mode=%s run=%s test=%s; real Test log:\n%s", mode, run.Status, step.Status, log)
		})
	}
}
