package steps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRealAntigravityCommitAttribution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell executable fixture")
	}
	for _, noVerify := range []bool{false, true} {
		t.Run(fmt.Sprint(noVerify), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			gitCmd(t, dir, "config", "user.name", "Victor I")
			gitCmd(t, dir, "config", "user.email", "vifarinde@gmail.com")
			hooks := filepath.Join(t.TempDir(), "hooks")
			if err := os.Mkdir(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nprintf 'Signed-off-by: Keeper <keeper@example.com>\\nCodex-Session: hook\\n' >> \"$1\"\nprintf 'ran\\n' >> \"$0.ran\"\n"
			for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
				if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, dir, "config", "core.hooksPath", hooks)
			root := t.TempDir()
			snapshot := filepath.Join(root, "stored-message")
			request := filepath.Join(root, "request")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			message := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <human@example.com>\nCo-Authored-By: gpt-5.5 <model@example.com>\nOriginated-by : bespoke-bot\nCodex-Session: agent"
			args := "commit --allow-empty"
			if noVerify {
				args += " --no-verify"
			}
			bin := filepath.Join(root, "agy")
			fake := "#!/bin/sh\nset -eu\nconversation=conv-fresh\nwhile test $# -gt 0; do\n if test \"$1\" = --conversation; then conversation=$2; shift; fi\n shift\ndone\n" +
				quote(testGitExecutable) + " " + args + " -m " + quote(message) + " >/dev/null\n" +
				quote(testGitExecutable) + " cat-file commit HEAD | sed '1,/^$/d' > " + quote(snapshot) + "\n" +
				"printf '%s' \"${NM_AGY_REQUEST-unset}\" > " + quote(request) + "\n" +
				"printf '%s\\n' '{\"event\":\"step_update\",\"step_update\":{\"text_delta\":\"stored-before-return\"}}'\n" +
				"printf '{\"event\":\"result\",\"conversation_id\":\"%s\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"done\"}}\\n' \"$conversation\"\n"
			if err := os.WriteFile(bin, []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			real, err := agent.New(types.AgentAntigravity, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer real.Close()
			c := newTestContextWithDBRecords(t, real, dir, base, head, config.Commands{})
			c.Config.Commit.Trailers = []string{"Originated-by: bespoke-bot"}
			want := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <human@example.com>\nSigned-off-by: Keeper <keeper@example.com>\n"
			if !noVerify {
				want += "Signed-off-by: Keeper <keeper@example.com>\n"
			}
			var session *agent.SessionRef
			for turn := 0; turn < 3; turn++ {
				opts := agent.RunOpts{CWD: dir, Prompt: "repair", Session: session, RequireCompletedChildren: true}
				if turn < 2 {
					opts.Env = []string{fmt.Sprintf("NM_AGY_REQUEST=turn-%d", turn)}
				}
				observed := false
				opts.OnChunk = func(chunk string) {
					if chunk != "stored-before-return" {
						return
					}
					observed = true
					got, err := os.ReadFile(snapshot)
					if err != nil || !bytes.Equal(got, []byte(want)) {
						t.Errorf("creation-time message before return = %q, %v; want %q", got, err, want)
					}
					requestValue, err := os.ReadFile(request)
					expected := "unset"
					if turn < 2 {
						expected = fmt.Sprintf("turn-%d", turn)
					}
					if err != nil || string(requestValue) != expected {
						t.Errorf("invocation environment = %q, %v; want %q", requestValue, err, expected)
					}
				}
				result, err := c.RunAgentContext(context.Background(), opts)
				if !observed {
					t.Error("did not inspect the committed message during the real adapter stream")
				}
				if err != nil {
					t.Fatalf("real adapter invocation/adoption = %v", err)
				}
				if result.SessionID != "conv-fresh" || result.Resumed != (turn == 1) {
					t.Fatalf("session identity = %#v", result)
				}
				if err := commitAgentFixes(c, types.StepReview, "", "fallback"); err != nil {
					t.Fatal(err)
				}
				if c.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
					t.Fatal("normal adoption did not record the real adapter commit")
				}
				if turn == 0 {
					session = &agent.SessionRef{ID: result.SessionID, Agent: "antigravity"}
				} else {
					session = nil
				}
			}
			for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
				data, err := os.ReadFile(filepath.Join(hooks, name+".ran"))
				if noVerify && name == "commit-msg" {
					if !os.IsNotExist(err) {
						t.Fatalf("no-verify ran commit-msg: %q, %v", data, err)
					}
				} else if err != nil || string(data) != strings.Repeat("ran\n", 3) {
					t.Fatalf("original hook effects = %q, %v", data, err)
				}
			}
		})
	}
}
