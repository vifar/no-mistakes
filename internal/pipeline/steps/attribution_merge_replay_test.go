package steps

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestMergeReplayCanonicalBytes(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(map[bool]string{false: "submitted", true: "imported"}[imported], func(t *testing.T) {
			f := newMergeFixture(t, false)
			submitted := f.headSHA
			gitCmd(t, f.dir, "checkout", "-b", "submitted-side", f.baseSHA)
			writeFixtureFile(t, f.dir, "side.txt", "submitted side work\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "submitted side change")
			gitCmd(t, f.dir, "checkout", "feature")
			gitCmd(t, f.dir, "merge", "--no-ff", "submitted-side", "-m", "submitted merge")
			message := []byte("  submitted merge subject  \n\nOriginal merge body.  \n# original comment\n\nCodex-Session: original-merge\n\n\n")
			canonical := amendReplayMessage(t, f.dir, message)
			f.headSHA = canonical
			if imported {
				gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/imported", canonical)
				gitCmd(t, f.dir, "reset", "--hard", submitted)
				f.headSHA = submitted
			}
			gitCmd(t, f.dir, "config", "rebase.rebaseMerges", "true")
			side := mutatingReplayHooks(t, f.dir)
			c := f.context(t, &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("clean replay called resolver")
				return nil, nil
			}}, "rebase")
			if imported {
				if err := rebaseWithAgent(c.Ctx, c, "origin/imported"); err != nil {
					t.Fatal(err)
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Fatal(err)
				}
			}
			for round := 0; round < 2; round++ {
				if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
				if got := parents(t, f.dir, "HEAD"); len(got) != 2 {
					t.Fatalf("not a recreated merge: parents=%v", got)
				}
				if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
					t.Errorf("merge replay bytes changed at creation: got %q, want %q", got, message)
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Error(err)
				}
				if round == 0 {
					gitCmd(t, f.dir, "checkout", "main")
					writeFixtureFile(t, f.dir, "next-target.txt", "advance target\n")
					gitCmd(t, f.dir, "add", "-A")
					gitCmd(t, f.dir, "commit", "-m", "advance target")
					gitCmd(t, f.dir, "push", "origin", "main")
					gitCmd(t, f.dir, "checkout", "feature")
				}
			}
			if got := storedReplayMessage(t, f.dir, canonical); !bytes.Equal(got, message) {
				t.Fatal("canonical object changed")
			}
			if effects, err := os.ReadFile(side); err != nil || !bytes.Contains(effects, []byte("prepare-commit-msg\n")) {
				t.Fatalf("hook effects: %q, %v", effects, err)
			}
		})
	}
}

func TestMergeReplayConflictContinuation(t *testing.T) {
	f := newMergeFixture(t, false)
	gitCmd(t, f.dir, "checkout", "-b", "side", f.baseSHA)
	writeFixtureFile(t, f.dir, "shared.txt", "side\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "side change")
	gitCmd(t, f.dir, "checkout", "feature")
	writeFixtureFile(t, f.dir, "shared.txt", "feature\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "feature shared change")
	if _, err := git.Run(context.Background(), f.dir, "merge", "--no-ff", "side"); err == nil {
		t.Fatal("fixture merge did not conflict")
	}
	writeFixtureFile(t, f.dir, "shared.txt", "resolved\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "resolved merge")
	message := []byte("  original resolved merge  \n\nCodex-Session: original\n\n\n")
	f.headSHA = amendReplayMessage(t, f.dir, message)
	gitCmd(t, f.dir, "config", "rebase.rebaseMerges", "true")
	side := mutatingReplayHooks(t, f.dir)
	turns := 0
	c := f.context(t, &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		turns++
		writeFixtureFile(t, f.dir, "shared.txt", "resolved\n")
		replayAgentGit(t, ctx, opts, "add", "-A")
		replayAgentGit(t, ctx, opts, "rebase", "--continue")
		return &agent.Result{}, nil
	}}, "rebase")
	for round := 0; round < 2; round++ {
		if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
			t.Fatal(err)
		}
		if turns != round+1 || len(parents(t, f.dir, "HEAD")) != 2 {
			t.Fatalf("merge continuation did not run: turns=%d", turns)
		}
		if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, message) {
			t.Fatalf("continued merge bytes = %q, want %q", got, message)
		}
		if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			gitCmd(t, f.dir, "checkout", "main")
			writeFixtureFile(t, f.dir, "next-target.txt", "advance target\n")
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "advance target")
			gitCmd(t, f.dir, "push", "origin", "main")
			gitCmd(t, f.dir, "checkout", "feature")
		}
	}

	if effects, err := os.ReadFile(side); err != nil || !bytes.Contains(effects, []byte("prepare-commit-msg\n")) {
		t.Fatalf("hook effects: %q, %v", effects, err)
	}
}
