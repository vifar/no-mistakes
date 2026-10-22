package steps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func storedReplayMessage(t *testing.T, dir, sha string) []byte {
	t.Helper()
	cmd := exec.Command(testGitExecutable, "cat-file", "commit", sha)
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	_, message, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		t.Fatal("commit object has no message boundary")
	}
	return message
}

func amendReplayMessage(t *testing.T, dir string, message []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(file, message, 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "commit", "--amend", "--cleanup=verbatim", "-F", file)
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if got := storedReplayMessage(t, dir, head); !bytes.Equal(got, message) {
		t.Fatalf("fixture message = %q, want %q", got, message)
	}
	return head
}

func mutatingReplayHooks(t *testing.T, dir string) string {
	t.Helper()
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(hooks, "effects")
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s' >> '%s'\nprintf '\\nHook body edit.\\n\\nSigned-off-by: Hook Person <hook@example.com>\\nCodex-Session: hook\\n' >> \"$1\"\n", name, side)
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "config", "core.hooksPath", hooks)
	return side
}

func TestReplayOriginalMessageBytes(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			f := newMergeFixture(t, conflict)
			message := []byte("  submitted raw subject  \n\nBody with trailing spaces.  \n# original comment\n\nDisclosure: fixtures were generated with the repository recorder.\nCo-Authored-By: Claude Smith <human@example.com>\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: original\n\n\n")
			f.headSHA = amendReplayMessage(t, f.dir, message)
			side := mutatingReplayHooks(t, f.dir)
			ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
				replayAgentGit(t, ctx, opts, "add", "-A")
				replayAgentGit(t, ctx, opts, "rebase", "--continue")
				return &agent.Result{}, nil
			}}
			c := f.context(t, ag, "rebase")
			if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
				t.Fatal(err)
			}
			if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
				t.Errorf("stored replay bytes = %q, want %q", got, message)
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Error(err)
			}
			if effects, err := os.ReadFile(side); err != nil || !bytes.Contains(effects, []byte("prepare-commit-msg\n")) {
				t.Fatalf("original hook side effect = %q, %v", effects, err)
			}
			c.Agent = &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "-C", f.headSHA)
				return &agent.Result{}, nil
			}}
			if _, err := c.RunAgent(agent.RunOpts{CWD: f.dir}); err != nil {
				t.Fatal(err)
			}
			got := storedReplayMessage(t, f.dir, "HEAD")
			if bytes.Contains(got, []byte("Codex-Session:")) || bytes.Contains(got, []byte("noreply@anthropic.com")) {
				t.Errorf("new copied correction retained attribution: %q", got)
			}
			if !bytes.Contains(got, []byte("Hook Person")) || !bytes.Contains(got, []byte("Hook body edit.")) {
				t.Errorf("new correction lost repository-hook message edits: %q", got)
			}
		})
	}
}

func TestImportedReplayOrderedIntegrations(t *testing.T) {
	for _, entry := range []string{"automatic", "with-agent", "automatic-no-hooks", "with-agent-no-hooks"} {
		t.Run(entry, func(t *testing.T) {
			f := newMergeFixture(t, false)
			c := f.context(t, &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("clean integration launched resolver")
				return nil, nil
			}}, "rebase")
			gitCmd(t, f.dir, "checkout", "-b", "remote-feature")
			writeFixtureFile(t, f.dir, "imported.txt", "imported history\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "temporary imported message")
			message := []byte("imported canonical message  \n\nDisclosure: fixtures were generated with the repository recorder.\nCo-Authored-By: Claude Smith <person@example.com>\nCodex-Session: imported\n\n\n")
			imported := amendReplayMessage(t, f.dir, message)
			gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/feature", imported)
			gitCmd(t, f.dir, "checkout", "feature")
			integrate := func(target string) {
				t.Helper()
				if strings.HasPrefix(entry, "automatic") {
					if conflicts, err := tryRebase(c.Ctx, c, target); err != nil || len(conflicts) > 0 {
						t.Fatalf("integration %s = %v, %v", target, conflicts, err)
					}
				} else if err := rebaseWithAgent(c.Ctx, c, target); err != nil {
					t.Fatal(err)
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Fatal(err)
				}
			}
			integrate("origin/feature")
			if c.Run.HeadSHA != imported {
				t.Fatal("remote feature was not imported by fast-forward")
			}
			side := ""
			if !strings.HasSuffix(entry, "-no-hooks") {
				side = mutatingReplayHooks(t, f.dir)
			}
			integrate("origin/main")
			if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
				t.Errorf("first imported replay = %q, want %q", got, message)
			}
			firstReplay := gitCmd(t, f.dir, "rev-parse", "HEAD")
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "next-target.txt", "advanced target\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "advance imported target")
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")
			integrate("origin/main")
			if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
				t.Errorf("successive imported replay = %q, want %q", got, message)
			}
			if gitCmd(t, f.dir, "rev-parse", "HEAD") == firstReplay {
				t.Fatal("second integration did not rewrite history")
			}
			if got := storedReplayMessage(t, f.dir, imported); !bytes.Equal(got, message) {
				t.Fatal("canonical imported object changed")
			}
			if side != "" {
				if effects, err := os.ReadFile(side); err != nil || len(effects) == 0 {
					t.Fatal("repository-hook side effects missing")
				}
			}
			c.Agent = &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "-m", "new correction\n\nCodex-Session: new")
				return &agent.Result{}, nil
			}}
			if _, err := c.RunAgent(agent.RunOpts{CWD: f.dir}); err != nil {
				t.Fatal(err)
			}
			if got := storedReplayMessage(t, f.dir, "HEAD"); bytes.Contains(got, []byte("Codex-Session:")) {
				t.Errorf("new correction not cleaned: %q", got)
			}
			if err := commitAgentFixes(c, "review", "", "fallback"); err != nil {
				t.Fatal(err)
			}
			clean := c.Run.HeadSHA
			gitCmd(t, f.dir, "-c", "core.hooksPath=", "commit", "--allow-empty", "--no-verify", "-m", "bypass\n\nCodex-Session: bypass")
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err == nil {
				t.Fatal("adopted attributed bypass")
			}
			if c.Run.HeadSHA != clean {
				t.Fatal("run adopted bypass head")
			}
		})
	}
}

func TestImportedTargetDivergentRebase(t *testing.T) {
	for _, entry := range []string{"automatic", "with-agent"} {
		t.Run(entry, func(t *testing.T) {
			f := newMergeFixture(t, false)
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "attributed-target.txt", "existing target work\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "existing target\n\nCodex-Session: upstream")
			target := gitCmd(t, f.dir, "rev-parse", "HEAD")
			expected := storedReplayMessage(t, f.dir, target)
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")
			c := f.context(t, &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("clean divergent rebase launched agent")
				return nil, nil
			}}, "rebase")
			if entry == "automatic" {
				if conflicts, err := tryRebase(c.Ctx, c, "origin/main"); err != nil || len(conflicts) > 0 {
					t.Fatalf("rebase = %v, %v", conflicts, err)
				}
			} else if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
				t.Fatal(err)
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Fatal(err)
			}
			if got := storedReplayMessage(t, f.dir, target); !bytes.Equal(got, expected) {
				t.Fatal("existing target message changed")
			}
			if got := gitCmd(t, f.dir, "rev-parse", "HEAD^"); got != target {
				t.Fatalf("divergent integration parent = %s, want %s", got, target)
			}
		})
	}
}
