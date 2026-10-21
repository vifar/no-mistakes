package steps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A file replaced by a directory (or the reverse) puts BOTH the removed path
// and its new descendant in the dirty set. Git's literal pathspec for the
// removed path also matches that descendant, so capturing one patch per dirty
// path concatenates the descendant's change twice and leaves a patch that
// cannot be applied after the worktree is gone. Recovery evidence exists to be
// usable, so every eligible change must appear exactly once.
func TestTargetRewindSnapshotCoversReplacedPathsOnce(t *testing.T) {
	c, _ := rewindFixture(t)
	c.RunLogDir = t.TempDir()
	head := c.Run.HeadSHA

	path := filepath.Join(c.WorkDir, "feature.txt")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "nested.txt"), []byte("nested replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "-A")

	if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("snapshot: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Paths     [][]byte                 `json:"paths"`
		Staged    []byte                   `json:"staged_patch"`
		Unstaged  []byte                   `json:"unstaged_patch"`
		Untracked []leftoverValidationFile `json:"untracked"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	// The dirty set legitimately names both paths; the patch must not repeat
	// the descendant for every pathspec that matches it.
	named := make([]string, 0, len(record.Paths))
	for _, p := range record.Paths {
		named = append(named, string(p))
	}
	if len(named) != 2 {
		t.Fatalf("dirty evidence must name both paths, got %q", named)
	}
	header := "diff --git a/feature.txt/nested.txt b/feature.txt/nested.txt"
	if got := strings.Count(string(record.Staged), header); got != 1 {
		t.Fatalf("the replacement's addition appears %d times in the recovery patch:\n%s", got, record.Staged)
	}
	if got := strings.Count(string(record.Staged), "deleted file mode"); got != 1 {
		t.Fatalf("the replacement's deletion appears %d times:\n%s", got, record.Staged)
	}

	// The proof that matters: the record restores the edits with the worktree gone.
	if err := os.RemoveAll(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	recovery := t.TempDir()
	gitCmd(t, recovery, "clone", c.Repo.UpstreamURL, ".")
	gitCmd(t, recovery, "checkout", "--detach", head)
	patch := filepath.Join(t.TempDir(), "staged.patch")
	if err := os.WriteFile(patch, record.Staged, 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, recovery, "apply", "--index", patch)
	if got, err := os.ReadFile(filepath.Join(recovery, "feature.txt", "nested.txt")); err != nil || string(got) != "nested replacement\n" {
		t.Fatalf("recovery lost the replacement: %q %v", got, err)
	}
	// The removed path must be recovered as removed, and the pathspec match
	// above (`git ls-files -- feature.txt` also lists its descendants) is why
	// each record is compared exactly.
	for _, tracked := range strings.Split(gitCmd(t, recovery, "ls-files", "-z"), "\x00") {
		if tracked == "feature.txt" {
			t.Fatal("recovery restored the removed file")
		}
	}
}

func TestTargetRewindSnapshotLargeScratchInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix filename contract")
	}
	c, _ := rewindFixture(t)
	c.RunLogDir = t.TempDir()
	head := c.Run.HeadSHA
	names := []string{"feature.txt", "trailing]", "trailing[", "trailing!", "trailing^", "trailing-", "trailing\\", "trailing?", "trailing*", "trailing\n", "literal[*?\\].txt"}
	if runtime.GOOS != "darwin" {
		names = append(names, "trailing\xff", "trailing\xfe")
	}
	for i := 0; i < 100; i++ {
		names = append(names, fmt.Sprintf("real-%03d-%s.txt", i, strings.Repeat("y", 180)))
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(c.WorkDir, name), []byte("staged "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, c.WorkDir, "add", "-A")
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(c.WorkDir, name), []byte("working "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	patternSibling := filepath.Join(c.WorkDir, `feature.tx[\t]`, ".journal-live", "postgres.log")
	if err := os.MkdirAll(filepath.Dir(patternSibling), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patternSibling, []byte("private scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "--literal-pathspecs", "add", `feature.tx[\t]/.journal-live/postgres.log`)
	scratch := filepath.Join(c.WorkDir, ".journal-live")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12000; i++ {
		name := fmt.Sprintf("%05d-%s.log", i, strings.Repeat("x", 200))
		if err := os.WriteFile(filepath.Join(scratch, name), []byte("private scratch\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("snapshot: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var record leftoverValidationSnapshot
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(record.StagedPatch, []byte(".journal-live")) || bytes.Contains(record.UnstagedPatch, []byte(".journal-live")) {
		t.Fatal("recovery patches retained scratch")
	}
	if len(record.Paths) != len(names) || len(record.Untracked) != 0 || bytes.Contains(record.Status, []byte(".journal-live")) {
		t.Fatalf("unexpected recovery scope: %+v", record)
	}
	if err := os.RemoveAll(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	recovery := t.TempDir()
	gitCmd(t, recovery, "clone", c.Repo.UpstreamURL, ".")
	gitCmd(t, recovery, "checkout", "--detach", head)
	for _, patch := range []struct {
		data []byte
		args []string
	}{
		{record.StagedPatch, []string{"apply", "--index"}},
		{record.UnstagedPatch, []string{"apply"}},
	} {
		file := filepath.Join(t.TempDir(), "recover.patch")
		if err := os.WriteFile(file, patch.data, 0600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, recovery, append(patch.args, file)...)
	}
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(recovery, name))
		if err != nil || string(got) != "working "+name+"\n" {
			t.Fatalf("working bytes not recovered for %q: %q %v", name, got, err)
		}
		if got := gitCmd(t, recovery, "show", ":"+name); got != "staged "+strings.TrimSpace(name) {
			t.Fatalf("staged bytes not recovered for %q: %q", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(recovery, ".journal-live")); !os.IsNotExist(err) {
		t.Fatalf("scratch restored by recovery: %v", err)
	}
}

// An intent-to-add path (`git add -N`) is a NEW file whose content is still
// untracked, so the scratch rules must classify it exactly like `??`. The
// measured case is a test harness's live database directory: it must never
// enter the recovery evidence, and it must not be reported as a real edit.
func TestTargetRewindSnapshotExcludesIntentToAddScratch(t *testing.T) {
	c, _ := rewindFixture(t)
	c.RunLogDir = t.TempDir()
	head := c.Run.HeadSHA
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("real edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(c.WorkDir, ".journal-live")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "postgres.log"), []byte("scratch bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "-N", ".journal-live/postgres.log")

	paths, err := leftoverDirtyPaths(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "feature.txt" {
		t.Fatalf("intent-to-add scratch counted as a real edit: %q", paths)
	}
	if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("snapshot: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Paths  [][]byte `json:"paths"`
		Status []byte   `json:"status"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Paths) != 1 || string(record.Paths[0]) != "feature.txt" {
		t.Fatalf("snapshot recorded scratch: %q", record.Paths)
	}
	if bytes.Contains(data, []byte("scratch bytes")) || bytes.Contains(record.Status, []byte(".journal-live")) {
		t.Fatal("snapshot captured scratch")
	}
}

func TestTargetRewindSnapshotReplacementExcludesScratch(t *testing.T) {
	for _, intentToAdd := range []bool{false, true} {
		name := "staged"
		if intentToAdd {
			name = "intent-to-add"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := rewindFixture(t)
			c.RunLogDir = t.TempDir()
			head := c.Run.HeadSHA
			parent := filepath.Join(c.WorkDir, "feature.txt")
			if err := os.Remove(parent); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(parent, ".journal-live"), 0700); err != nil {
				t.Fatal(err)
			}
			for path, content := range map[string]string{
				"real[1].txt":                "replacement content\n",
				".journal-live/postgres.log": "private scratch content\n",
			} {
				if err := os.WriteFile(filepath.Join(parent, path), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if intentToAdd {
				gitCmd(t, c.WorkDir, "add", "-N", "-A")
			} else {
				gitCmd(t, c.WorkDir, "add", "-A")
			}
			if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("snapshot: %v %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var record leftoverValidationSnapshot
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			for _, patch := range [][]byte{record.StagedPatch, record.UnstagedPatch} {
				if bytes.Contains(patch, []byte("private scratch content")) || bytes.Contains(patch, []byte(".journal-live")) {
					t.Fatalf("recovery patch retained excluded scratch: %s", patch)
				}
			}
			if bytes.Contains(record.Status, []byte(".journal-live")) || len(record.Untracked) != 0 || len(record.Paths) != 2 {
				t.Fatalf("unexpected recovery scope: %+v", record)
			}
			if err := os.RemoveAll(c.WorkDir); err != nil {
				t.Fatal(err)
			}
			recovery := t.TempDir()
			gitCmd(t, recovery, "clone", c.Repo.UpstreamURL, ".")
			gitCmd(t, recovery, "checkout", "--detach", head)
			for _, patch := range [][]byte{record.StagedPatch, record.UnstagedPatch} {
				if len(patch) == 0 {
					continue
				}
				file := filepath.Join(t.TempDir(), "recover.patch")
				if err := os.WriteFile(file, patch, 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, recovery, "apply", "--index", file)
			}
			got, err := os.ReadFile(filepath.Join(recovery, "feature.txt", "real[1].txt"))
			if err != nil || string(got) != "replacement content\n" {
				t.Fatalf("replacement not recovered exactly once: %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(recovery, "feature.txt", ".journal-live")); !os.IsNotExist(err) {
				t.Fatalf("scratch restored by recovery: %v", err)
			}
		})
	}
}
