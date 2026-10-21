package steps

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIntegrationUsesPinnedTargetWhenRefAdvances(t *testing.T) {
	for _, path := range []string{"rebase", "rebase-agent", "merge", "merge-agent"} {
		for _, mode := range []string{"divergent", "fast-forward", "conflict"} {
			if mode == "conflict" && !strings.HasSuffix(path, "-agent") {
				continue
			}
			t.Run(path+"/"+mode, func(t *testing.T) {
				f := newMergeFixture(t, mode == "conflict")
				if mode == "fast-forward" {
					gitCmd(t, f.dir, "reset", "--hard", f.baseSHA)
					f.headSHA = f.baseSHA
				}
				targetA := f.mainSHA
				aMessage := storedReplayMessage(t, f.dir, targetA)
				gitCmd(t, f.dir, "checkout", "main")
				writeFixtureFile(t, f.dir, "later-target.txt", "later imported work\n")
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "later target\n\nCodex-Session: existing-target")
				targetB := gitCmd(t, f.dir, "rev-parse", "HEAD")
				bMessage := storedReplayMessage(t, f.dir, targetB)
				gitCmd(t, f.dir, "checkout", "feature")
				gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/main", targetA)
				calls := 0
				resolver := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					calls++
					writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
					replayAgentGit(t, ctx, opts, "add", "-A")
					if strings.HasPrefix(path, "merge") {
						replayAgentGit(t, ctx, opts, "commit", "--no-edit")
					} else {
						replayAgentGit(t, ctx, opts, "rebase", "--continue")
					}
					return &agent.Result{}, nil
				}}
				c := f.context(t, resolver, "rebase")
				advanced := false
				originalLog := c.Log
				c.Log = func(line string) {
					originalLog(line)
					if !advanced && (strings.HasPrefix(line, "rebasing onto origin/main") || strings.HasPrefix(line, "merging origin/main") || strings.HasPrefix(line, "fast-forwarding to origin/main")) {
						gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/main", targetB)
						advanced = true
					}
				}
				var err error
				switch path {
				case "rebase":
					var conflicts []string
					conflicts, err = tryRebase(c.Ctx, c, "origin/main")
					if len(conflicts) > 0 {
						t.Fatalf("unexpected conflicts: %v", conflicts)
					}
				case "rebase-agent":
					err = rebaseWithAgent(c.Ctx, c, "origin/main")
				case "merge":
					var conflicts []string
					conflicts, err = tryMerge(c.Ctx, c, "origin/main")
					if len(conflicts) > 0 {
						t.Fatalf("unexpected conflicts: %v", conflicts)
					}
				case "merge-agent":
					err = mergeWithAgent(c.Ctx, c, "origin/main")
				}
				if err != nil {
					t.Fatalf("integration against moving ref: %v", err)
				}
				if !advanced {
					t.Fatal("fixture never advanced the shared target ref")
				}
				if !isAncestor(c.Ctx, f.dir, targetA, "HEAD") {
					t.Error("did not integrate pinned target A")
				}
				if isAncestor(c.Ctx, f.dir, targetB, "HEAD") {
					t.Error("integrated target B after pinning only A")
				}
				if mode == "conflict" && calls != 1 {
					t.Fatalf("conflict resolver calls = %d", calls)
				}
				if mode != "conflict" && calls != 0 {
					t.Fatalf("unexpected resolver call: %d", calls)
				}
				if mode == "fast-forward" && gitCmd(t, f.dir, "rev-parse", "HEAD") != targetA {
					t.Error("fast-forward used a different target")
				}
				if strings.HasPrefix(path, "merge") && mode != "fast-forward" {
					ps := parents(t, f.dir, "HEAD")
					if len(ps) != 2 || ps[0] != f.headSHA || ps[1] != targetA {
						t.Errorf("merge parents = %v, want [%s %s]", ps, f.headSHA, targetA)
					}
				}
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
					t.Fatal(err)
				}
				run := func(args ...string) (string, error) { return git.Run(c.Ctx, f.dir, args...) }
				if err := c.ValidateCommitAttribution(c.Run.HeadSHA, targetB, run); err == nil {
					t.Error("the unselected attributed B was exempted")
				}
				if !bytes.Equal(storedReplayMessage(t, f.dir, targetA), aMessage) || !bytes.Equal(storedReplayMessage(t, f.dir, targetB), bMessage) {
					t.Fatal("imported objects changed")
				}
				recorded := c.Run.HeadSHA
				gitCmd(t, f.dir, "-c", "core.hooksPath=", "commit", "--allow-empty", "-m", fmt.Sprintf("new correction %s\n\nCodex-Session: new", path))
				bypass := gitCmd(t, f.dir, "rev-parse", "HEAD")
				if _, err := updateHeadSHA(c.Ctx, c, "main"); err == nil {
					t.Error("new attributed work was exempted")
				}
				stored, err := c.DB.GetRun(c.Run.ID)
				if err != nil || stored.HeadSHA != recorded || c.Run.HeadSHA != recorded || gitCmd(t, f.dir, "rev-parse", "HEAD") != bypass {
					t.Fatalf("bypass refusal changed head state: %#v, %v", stored, err)
				}
			})
		}
	}
}

func TestPinnedIntegrationMissingTargetKeepsRewindGuard(t *testing.T) {
	c, selected := rewindFixture(t)
	before := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	skip, target, err := shouldSkipRebase(c.Ctx, c, "origin/missing")
	if err != nil || !skip || target != "" {
		t.Fatalf("ordinary missing target = %v, %q, %v", skip, target, err)
	}
	advanceRewindTarget(t, c)
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	skip, target, err = shouldSkipRebase(c.Ctx, c, "origin/missing")
	if err == nil || skip || target != "" || !strings.Contains(err.Error(), "resolve checked rewind target") {
		t.Fatalf("checked missing target = %v, %q, %v", skip, target, err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("missing-target resolution moved head: %s", got)
	}
}
