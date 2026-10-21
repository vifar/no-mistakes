package steps

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// Recovery's wire contract uses base64 byte fields for every filesystem name
// and symlink target. JSON strings cannot represent arbitrary Unix path bytes.
type leftoverRecoveryRecord struct {
	Head          string   `json:"head"`
	Paths         [][]byte `json:"paths"`
	StagedPatch   []byte   `json:"staged_patch"`
	UnstagedPatch []byte   `json:"unstaged_patch"`
	Untracked     []struct {
		Path          []byte `json:"path"`
		Data          []byte `json:"data"`
		SymlinkTarget []byte `json:"symlink_target"`
		Mode          uint32 `json:"mode"`
	} `json:"untracked"`
}

func readLeftoverRecoveryRecord(t *testing.T, dir string, record any) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "leftover-validation-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("snapshot: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, record); err != nil {
		t.Fatal(err)
	}
}

// The oracle is the original file contents, modes and readlink bytes. Rebuild
// from serialized evidence after cleanup, so lossy JSON cannot pass unnoticed.
func TestTargetRewindSnapshotRecoversFilenameAndSymlinkBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix filename byte contract")
	}
	c, _ := rewindFixture(t)
	c.RunLogDir = t.TempDir()
	contents := map[string][]byte{"x\xff": {0, 255, 1}, "x\xfe": {0, 254, 2}, "empty": {}}
	if runtime.GOOS == "darwin" {
		// APFS rejects non-UTF-8 filenames; the Git-index test below exercises
		// their real capture here without asking the filesystem to create them.
		contents = map[string][]byte{"x-one": {0, 255, 1}, "x-two": {0, 254, 2}, "empty": {}}
	}
	links := map[string]string{"link-one": "target\xff", "link-two": "target\xfe"}
	for path, data := range contents {
		if err := os.WriteFile(filepath.Join(c.WorkDir, path), data, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(c.WorkDir, path), 0750); err != nil {
			t.Fatal(err)
		}
	}
	for path, target := range links {
		if err := os.Symlink(target, filepath.Join(c.WorkDir, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshotLeftoverValidationEdits(c, "test", c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	var record leftoverRecoveryRecord
	readLeftoverRecoveryRecord(t, c.RunLogDir, &record)
	recovery := t.TempDir()
	gotPaths := map[string]bool{}
	for _, path := range record.Paths {
		gotPaths[string(path)] = true
	}
	wantPaths := map[string]bool{}
	for path := range contents {
		wantPaths[path] = true
	}
	for path := range links {
		wantPaths[path] = true
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("filename bytes lost: %q", record.Paths)
	}
	if len(record.Untracked) != len(wantPaths) {
		t.Fatalf("lost untracked entries: got %d, want %d", len(record.Untracked), len(wantPaths))
	}
	for _, file := range record.Untracked {
		path := filepath.Join(recovery, string(file.Path))
		if file.SymlinkTarget != nil {
			if err := os.Symlink(string(file.SymlinkTarget), path); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(path, file.Data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, os.FileMode(file.Mode)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for path, want := range contents {
		got, err := os.ReadFile(filepath.Join(recovery, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("file %q not recovered: %x %v", path, got, err)
		}
		info, err := os.Stat(filepath.Join(recovery, path))
		if err != nil || info.Mode().Perm() != 0750 {
			t.Errorf("mode %q not recovered: %v %v", path, info, err)
		}
	}
	for path, want := range links {
		got, err := os.Readlink(filepath.Join(recovery, path))
		if err != nil || got != want {
			t.Errorf("link %q not recovered: %q %v", path, got, err)
		}
	}
}

// Git can represent byte filenames in its index even on filesystems that
// reject them. Distinct names must survive JSON and remain usable in patches.
func TestTargetRewindSnapshotRecoversGitFilenameBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix filename byte contract")
	}
	c, _ := rewindFixture(t)
	c.RunLogDir = t.TempDir()
	names := []string{"x\xff", "x\xfe"}
	blob := gitCmd(t, c.WorkDir, "rev-parse", "HEAD:feature.txt")
	for _, name := range names {
		gitCmd(t, c.WorkDir, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	}
	gitCmd(t, c.WorkDir, "commit", "-m", "record byte filenames")
	head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, c.WorkDir, "push", "origin", "HEAD:refs/heads/byte-filenames")
	for _, name := range names {
		gitCmd(t, c.WorkDir, "update-index", "--force-remove", name)
	}
	if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	var record leftoverRecoveryRecord
	readLeftoverRecoveryRecord(t, c.RunLogDir, &record)
	gotPaths := map[string]bool{}
	for _, path := range record.Paths {
		gotPaths[string(path)] = true
	}
	if !reflect.DeepEqual(gotPaths, map[string]bool{names[0]: true, names[1]: true}) {
		t.Fatalf("Git filename bytes lost: %q", record.Paths)
	}
	recovery := t.TempDir()
	gitCmd(t, recovery, "clone", "--no-checkout", c.Repo.UpstreamURL, ".")
	gitCmd(t, recovery, "read-tree", record.Head)
	patch := filepath.Join(t.TempDir(), "byte-names.patch")
	if err := os.WriteFile(patch, record.StagedPatch, 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, recovery, "apply", "--cached", patch)
	if got := gitCmd(t, recovery, "ls-files", "--", names[0], names[1]); got != "" {
		t.Fatalf("index deletions not recovered: %q", got)
	}
}

// The oracle is git apply plus the resulting gitlink SHA, for BOTH patch
// streams. Display config must neither hide the edit nor turn it into prose.
func TestTargetRewindSnapshotRecoversGitlinkUnderDiffConfig(t *testing.T) {
	for _, state := range []string{"staged", "unstaged"} {
		for _, setting := range []string{"ignore", "log", "both"} {
			t.Run(state+"/"+setting, func(t *testing.T) {
				c, _ := rewindFixture(t)
				c.RunLogDir = t.TempDir()
				old, next := c.Run.BaseSHA, c.Run.HeadSHA
				gitCmd(t, c.WorkDir, "update-index", "--add", "--cacheinfo", "160000,"+old+",module")
				gitCmd(t, c.WorkDir, "commit", "-m", "record gitlink")
				head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "push", "origin", "HEAD:refs/heads/snapshot-gitlink")
				module := filepath.Join(c.WorkDir, "module")
				gitCmd(t, c.WorkDir, "clone", c.Repo.UpstreamURL, module)
				gitCmd(t, module, "checkout", "--detach", next)
				if state == "staged" {
					gitCmd(t, c.WorkDir, "update-index", "--cacheinfo", "160000,"+next+",module")
				}
				if setting != "log" {
					gitCmd(t, c.WorkDir, "config", "diff.ignoreSubmodules", "all")
				}
				if setting != "ignore" {
					gitCmd(t, c.WorkDir, "config", "diff.submodule", "log")
				}
				if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(c.WorkDir); err != nil {
					t.Fatal(err)
				}
				var record struct {
					Head          string `json:"head"`
					StagedPatch   []byte `json:"staged_patch"`
					UnstagedPatch []byte `json:"unstaged_patch"`
				}
				readLeftoverRecoveryRecord(t, c.RunLogDir, &record)
				patchData := record.StagedPatch
				if state == "unstaged" {
					patchData = record.UnstagedPatch
				}
				if len(patchData) == 0 {
					t.Fatal("gitlink change lost from recovery patch")
				}
				recovery := t.TempDir()
				gitCmd(t, recovery, "clone", "--no-checkout", c.Repo.UpstreamURL, ".")
				gitCmd(t, recovery, "read-tree", record.Head)
				patch := filepath.Join(t.TempDir(), "gitlink.patch")
				if err := os.WriteFile(patch, patchData, 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, recovery, "apply", "--cached", patch)
				if got := gitCmd(t, recovery, "rev-parse", ":module"); got != next {
					t.Fatalf("gitlink not recovered: %s, want %s", got, next)
				}
			})
		}
	}
}
