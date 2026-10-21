package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// leftoverValidationSnapshot is private recovery evidence, not a Test artifact.
// Byte fields preserve binary patches, unusual filenames and untracked contents
// without relying on the disposable worktree (or its Git object store) surviving.
// Apply staged_patch to head, then unstaged_patch to that index version;
// untracked holds files that Git's diff cannot represent until they are added.
type leftoverValidationSnapshot struct {
	RunID         string            `json:"run_id"`
	Producer      string            `json:"producer"`
	Head          string            `json:"head"`
	Paths         []string          `json:"paths"`
	Status        []byte            `json:"status"`
	StagedPatch   []byte            `json:"staged_patch"`
	UnstagedPatch []byte            `json:"unstaged_patch"`
	Untracked     map[string][]byte `json:"untracked,omitempty"`
	Symlinks      map[string]string `json:"symlinks,omitempty"`
	Modes         map[string]uint32 `json:"modes,omitempty"`
}

func snapshotLeftoverValidationEdits(c *pipeline.StepContext, producer, head string) error {
	if c.RunLogDir == "" {
		return nil
	} // focused embeddings without diagnostics
	paths, err := leftoverDirtyPaths(c)
	if err != nil || len(paths) == 0 {
		return err
	}
	// Git can report a staged deletion and an untracked survivor for one path.
	// Keep every status record, but capture each path's patches only once.
	wanted := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if !wanted[path] {
			wanted[path] = true
			unique = append(unique, path)
		}
	}
	paths = unique
	snapshot := leftoverValidationSnapshot{RunID: c.Run.ID, Producer: producer, Head: head, Paths: paths,
		Untracked: map[string][]byte{}, Symlinks: map[string]string{}, Modes: map[string]uint32{}}
	status, err := git.RunRaw(c.Ctx, c.WorkDir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	entries := map[string][]string{}
	for _, entry := range strings.Split(string(status), "\x00") {
		if len(entry) >= 4 && entry[2] == ' ' && wanted[entry[3:]] {
			entries[entry[3:]] = append(entries[entry[3:]], entry)
			snapshot.Status = append(snapshot.Status, []byte(entry+"\x00")...)
		}
	}
	for _, path := range paths {
		records := entries[path]
		if len(records) == 0 {
			return fmt.Errorf("dirty path changed while recording evidence: %q", path)
		}
		untracked := false
		for _, entry := range records {
			untracked = untracked || strings.HasPrefix(entry, "?? ")
		}
		// Literal pathspecs and disabled external diff/textconv prevent source
		// filenames or repository diff drivers from changing the captured scope.
		// Recovery consumes a standard patch, regardless of display preferences.
		patchArgs := []string{"--literal-pathspecs", "diff", "--no-color", "--binary",
			"--no-ext-diff", "--no-textconv", "--no-renames", "--no-relative",
			"--src-prefix=a/", "--dst-prefix=b/", "--unified=3",
			"--output-indicator-new=+", "--output-indicator-old=-", "--output-indicator-context= "}
		staged, err := git.RunRaw(c.Ctx, c.WorkDir, append(patchArgs, "--cached", head, "--", path)...)
		if err != nil {
			return err
		}
		snapshot.StagedPatch = append(snapshot.StagedPatch, staged...)
		unstaged, err := git.RunRaw(c.Ctx, c.WorkDir, append(patchArgs, "--", path)...)
		if err != nil {
			return err
		}
		snapshot.UnstagedPatch = append(snapshot.UnstagedPatch, unstaged...)
		if !untracked {
			continue
		}
		full := filepath.Join(c.WorkDir, filepath.FromSlash(path))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		snapshot.Modes[path] = uint32(info.Mode().Perm())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			snapshot.Symlinks[path] = target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			snapshot.Untracked[path] = data
		default:
			return fmt.Errorf("cannot preserve non-file leftover path %q", path)
		}
	}
	if err := os.MkdirAll(c.RunLogDir, 0700); err != nil {
		return err
	}
	// Unique snapshots retain every attempt across conflict retries and recovery.
	// The final name is visible only after the complete private record is closed.
	f, err := os.CreateTemp(c.RunLogDir, "leftover-validation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	encodeErr := json.NewEncoder(f).Encode(snapshot)
	closeErr := f.Close()
	if encodeErr != nil {
		return encodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	destination := strings.TrimSuffix(f.Name(), ".tmp") + ".json"
	if err := os.Rename(f.Name(), destination); err != nil {
		return err
	}
	if c.Log != nil {
		c.Log(fmt.Sprintf("saved leftover %s validation edits for paths %q in private run evidence %s", producer, paths, filepath.Base(destination)))
	}
	return nil
}
