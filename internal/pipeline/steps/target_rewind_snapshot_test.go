package steps

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is issue #113: the producing step and the actual edits must
// remain inspectable after the disposable worktree has gone away.
func TestTargetRewindDurableLeftoverEvidence(t *testing.T) {
	for _, kind := range []string{"unstaged", "partial stage", "untracked binary", "protected refusal", "spent budget", "conflict", "clean"} {
		t.Run(kind, func(t *testing.T) {
			c, selected := rewindFixture(t)
			c.RunLogDir = t.TempDir()
			if kind == "conflict" {
				gitCmd(t, c.WorkDir, "checkout", "main")
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("target conflict\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", "feature.txt")
				gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target")
				gitCmd(t, c.WorkDir, "push", "origin", "main")
				gitCmd(t, c.WorkDir, "checkout", "feature")
			} else {
				advanceRewindTarget(t, c)
			}
			c.PRContextAfterStep = true
			if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
				t.Fatal(err)
			}
			// This is the executor's durable post-step producer evidence.
			if err := c.DB.RecordTargetRewindBoundary(c.Run.ID, 1, "test", `{}`); err != nil {
				t.Fatal(err)
			}
			if kind != "clean" {
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("staged version\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "partial stage" {
					gitCmd(t, c.WorkDir, "add", "feature.txt")
				}
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("leftover version\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			binary := []byte{0, 255, 1, 2}
			if kind == "untracked binary" {
				if err := os.WriteFile(filepath.Join(c.WorkDir, "new\nfile.bin"), binary, 0600); err != nil {
					t.Fatal(err)
				}
			}
			scratch := filepath.Join(c.WorkDir, ".journal-live")
			if err := os.Mkdir(scratch, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scratch, "scratch.log"), []byte("scratch bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "protected refusal" {
				c.Config.ProtectedPaths = []string{"feature.txt"}
			}
			if kind == "spent budget" {
				if err := c.DB.RecordLeftoverValidationCommit(c.Run.ID, c.Run.HeadSHA); err != nil {
					t.Fatal(err)
				}
			}
			c.PRContextAfterStep = false
			out, err := (&RebaseStep{}).Execute(c)
			switch kind {
			case "protected refusal", "spent budget":
				if err == nil || !strings.Contains(err.Error(), "test step left uncommitted edits") {
					t.Fatalf("missing attributed refusal: %+v %v", out, err)
				}
			case "conflict":
				if err != nil || out == nil || !out.NeedsApproval || !strings.Contains(out.Findings, "feature.txt") {
					t.Fatalf("real conflict was hidden: %+v %v", out, err)
				}
			default:
				if err != nil || out == nil || out.NeedsApproval {
					t.Fatalf("rewind failed: %+v %v", out, err)
				}
			}
			files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "clean" {
				if len(files) != 0 {
					t.Fatalf("clean rewind produced snapshots: %v", files)
				}
				return
			}
			if len(files) != 1 {
				t.Fatalf("missing private run snapshot: %v", files)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var snapshot struct {
				RunID         string            `json:"run_id"`
				Producer      string            `json:"producer"`
				Head          string            `json:"head"`
				Paths         []string          `json:"paths"`
				Status        []byte            `json:"status"`
				StagedPatch   []byte            `json:"staged_patch"`
				UnstagedPatch []byte            `json:"unstaged_patch"`
				Untracked     map[string][]byte `json:"untracked"`
			}
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.RunID != c.Run.ID || snapshot.Producer != "test" || len(snapshot.Head) != 40 || len(snapshot.Paths) == 0 || !bytes.Contains(snapshot.Status, []byte("feature.txt")) {
				t.Fatalf("snapshot lacks execution evidence: %+v", snapshot)
			}
			if !bytes.Contains(snapshot.UnstagedPatch, []byte("+leftover version")) {
				t.Fatal("snapshot lost unstaged edit")
			}
			if kind == "partial stage" && !bytes.Contains(snapshot.StagedPatch, []byte("+staged version")) {
				t.Fatal("snapshot lost index version")
			}
			if kind == "untracked binary" && !bytes.Equal(snapshot.Untracked["new\nfile.bin"], binary) {
				t.Fatal("snapshot lost untracked binary bytes or filename")
			}
			if bytes.Contains(data, []byte("scratch bytes")) || bytes.Contains(snapshot.Status, []byte(".journal-live")) {
				t.Fatal("snapshot captured scratch")
			}
			info, err := os.Stat(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("private snapshot mode: %o", info.Mode().Perm())
			}
			if err := os.RemoveAll(c.WorkDir); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(files[0]); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("worktree removal lost evidence: %v", err)
			}
			// Recover both index and working copies against the recorded head.
			// Checking that patches apply catches a snapshot that merely names
			// the edit without keeping usable recovery evidence.
			recovery := t.TempDir()
			gitCmd(t, recovery, "clone", c.Repo.UpstreamURL, ".")
			gitCmd(t, recovery, "checkout", "--detach", snapshot.Head)
			for _, patch := range []struct {
				name  string
				data  []byte
				index bool
			}{{"staged.patch", snapshot.StagedPatch, true}, {"unstaged.patch", snapshot.UnstagedPatch, false}} {
				if len(patch.data) == 0 {
					continue
				}
				file := filepath.Join(t.TempDir(), patch.name)
				if err := os.WriteFile(file, patch.data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"apply"}
				if patch.index {
					args = append(args, "--index")
				}
				gitCmd(t, recovery, append(args, file)...)
			}
			if got, err := os.ReadFile(filepath.Join(recovery, "feature.txt")); err != nil || string(got) != "leftover version\n" {
				t.Fatalf("patches did not recover the working copy: %q %v", got, err)
			}
			if kind == "partial stage" {
				if got := gitCmd(t, recovery, "show", ":feature.txt"); got != "staged version" {
					t.Fatalf("patches lost the partial index: %q", got)
				}
			}
		})
	}
}

// Refuse before touching the edits when their recovery record cannot be saved.
func TestTargetRewindEvidenceFailurePreservesTheWorktree(t *testing.T) {
	c, selected := rewindFixture(t)
	advanceRewindTarget(t, c)
	c.PRContextAfterStep = true
	if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.RecordTargetRewindBoundary(c.Run.ID, 1, "test", `{}`); err != nil {
		t.Fatal(err)
	}
	c.RunLogDir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(c.RunLogDir, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.WorkDir, "feature.txt")
	if err := os.WriteFile(path, []byte("must survive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	c.PRContextAfterStep = false
	_, err := (&RebaseStep{}).Execute(c)
	if err == nil || !strings.Contains(err.Error(), "save leftover validation evidence") || !strings.Contains(err.Error(), "test step left uncommitted edits") || !strings.Contains(err.Error(), "feature.txt") {
		t.Fatalf("missing attributed evidence failure: %v", err)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("evidence failure moved head: %s", got)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "must survive\n" {
		t.Fatalf("evidence failure lost edits: %q %v", got, err)
	}
}
