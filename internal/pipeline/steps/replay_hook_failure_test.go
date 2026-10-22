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
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestReplayMappingAfterOriginalHookFailure(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(fmt.Sprint(imported), func(t *testing.T) {
			f := newMergeFixture(t, false)
			message := []byte("  protected message  \n\nBody.  \n# original comment\n\nCodex-Session: protected\n\n\n")
			canonical := amendReplayMessage(t, f.dir, message)
			f.headSHA = canonical
			c := f.context(t, &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("clean replay invoked agent")
				return nil, nil
			}}, "rebase")
			if imported {
				gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/imported", canonical)
				gitCmd(t, f.dir, "reset", "--hard", f.baseSHA)
				c.Run.HeadSHA = f.baseSHA
				c.Run.SubmittedHeadSHA = &f.baseSHA
				if err := rebaseWithAgent(c.Ctx, c, "origin/imported"); err != nil {
					t.Fatal(err)
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Fatal(err)
				}
			}
			hooks := filepath.Join(t.TempDir(), "hooks")
			if err := os.Mkdir(hooks, 0700); err != nil {
				t.Fatal(err)
			}
			effects := filepath.Join(hooks, "rewrite")
			script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + effects + ".args'\ncat >> '" + effects + "'\nexit 37\n"
			if err := os.WriteFile(filepath.Join(hooks, "post-rewrite"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "config", "core.hooksPath", hooks)
			for round := 0; round < 2; round++ {
				old := gitCmd(t, f.dir, "rev-parse", "HEAD")
				if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
				next := gitCmd(t, f.dir, "rev-parse", "HEAD")
				if next == old {
					t.Fatal("rebase did not rewrite")
				}
				if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
					t.Errorf("protected bytes changed: %q", got)
				}
				if data, err := os.ReadFile(effects); err != nil || !bytes.Contains(data, []byte(old+" "+next+"\n")) {
					t.Errorf("original hook input = %q %v", data, err)
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Errorf("normal replay adoption failed: %v", err)
				}
				metadata := gitCmd(t, f.dir, "rev-parse", "--absolute-git-dir")
				if data, err := os.ReadFile(filepath.Join(metadata, "no-mistakes-attribution", "replays", next)); err != nil || strings.TrimSpace(string(data)) != canonical {
					t.Errorf("canonical mapping = %q %v", data, err)
				}
				if round == 0 {
					gitCmd(t, f.dir, "checkout", "main")
					writeFixtureFile(t, f.dir, "next.txt", "next target\n")
					gitCmd(t, f.dir, "add", "-A")
					gitCmd(t, f.dir, "commit", "-m", "advance target")
					gitCmd(t, f.dir, "push", "origin", "main")
					gitCmd(t, f.dir, "checkout", "feature")
				}
			}
			env, err := c.CommitAttributionEnv(nil, func(args ...string) (string, error) { return git.Run(c.Ctx, f.dir, args...) })
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(input, []byte(canonical+" "+gitCmd(t, f.dir, "rev-parse", "HEAD")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(testGitExecutable, "hook", "run", "--to-stdin="+input, "post-rewrite", "--", "rebase")
			cmd.Dir, cmd.Env = f.dir, append(os.Environ(), env...)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("direct hook failure swallowed: %s", out)
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
				t.Errorf("direct hook exit = %v, want 37", err)
			}
		})
	}
}
