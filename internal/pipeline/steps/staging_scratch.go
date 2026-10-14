package steps

import (
	"fmt"
	"path"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// maxListedPaths caps how many offending paths a refusal or log line names.
const maxListedPaths = 20

// stagingBatchBytes bounds the pathspec bytes handed to one `git add`, well
// under the Windows command-line limit.
const stagingBatchBytes = 16000

// knownConfigDirs are hidden directories that hold committed project
// configuration rather than tool scratch. A new one is still staged and
// published; every other hidden directory that is new is treated as scratch.
var knownConfigDirs = map[string]bool{
	".github":       true,
	".gitlab":       true,
	".circleci":     true,
	".devcontainer": true,
	".vscode":       true,
	".husky":        true,
	".changeset":    true,
	".storybook":    true,
}

// isScratchDirName reports whether a directory name marks tool scratch when the
// directory is new: node_modules, or a hidden directory that is not a known
// project-configuration directory.
func isScratchDirName(name string) bool {
	if name == "node_modules" {
		return true
	}
	return len(name) > 1 && strings.HasPrefix(name, ".") && name != ".." && !knownConfigDirs[name]
}

func matchesStagingExclude(sctx *pipeline.StepContext, file string) (string, bool) {
	file = strings.TrimSuffix(file, "/")
	for _, pattern := range sctx.Config.StagingExclude {
		if matchIgnorePattern(file, pattern) {
			return pattern, true
		}
	}
	return "", false
}

// firstScratchComponent returns the shortest leading directory prefix of rel
// (a slash path) whose last component is scratch, or "". dirPath reports that
// rel itself names a directory, so its final component counts too.
func firstScratchComponent(rel string, dirPath bool) string {
	parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
	if !dirPath {
		parts = parts[:len(parts)-1]
	}
	for i, part := range parts {
		if isScratchDirName(part) {
			return strings.Join(parts[:i+1], "/") + "/"
		}
	}
	return ""
}

// stageUntrackedChanges stages untracked files individually. A wholly
// untracked directory (git's --directory collapse point) is skipped when its
// name marks tool scratch, at any depth, so a populated .cache, .test-fixture
// or node_modules never rides into an automatic commit. A new file inside an
// already-tracked hidden directory is listed as a file and is staged. Entries
// matching staging_exclude are skipped too. Skips are logged once.
func stageUntrackedChanges(sctx *pipeline.StepContext) error {
	listed, err := stepGitRunRaw(sctx, "ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err != nil {
		return fmt.Errorf("list untracked files: %w", err)
	}
	var toAdd, skipped []string
	skippedSeen := map[string]bool{}
	for _, entry := range nulFields(listed) {
		if !strings.HasSuffix(entry, "/") {
			if _, excluded := matchesStagingExclude(sctx, entry); excluded {
				skipped = appendUnique(skipped, skippedSeen, entry)
				continue
			}
			toAdd = append(toAdd, entry)
			continue
		}
		if isScratchDirName(path.Base(strings.TrimSuffix(entry, "/"))) {
			skipped = appendUnique(skipped, skippedSeen, entry)
			continue
		}
		// A wholly untracked directory is expanded so nested scratch and
		// staging_exclude matches inside it are still honoured.
		files, err := stepGitRunRaw(sctx, "--literal-pathspecs", "ls-files", "--others", "--exclude-standard", "-z", "--", entry)
		if err != nil {
			return fmt.Errorf("list untracked files in %s: %w", entry, err)
		}
		for _, file := range nulFields(files) {
			rel := strings.TrimPrefix(file, entry)
			if scratch := firstScratchComponent(rel, strings.HasSuffix(file, "/")); scratch != "" {
				skipped = appendUnique(skipped, skippedSeen, entry+scratch)
				continue
			}
			if _, excluded := matchesStagingExclude(sctx, file); excluded {
				skipped = appendUnique(skipped, skippedSeen, file)
				continue
			}
			toAdd = append(toAdd, file)
		}
	}
	for _, batch := range pathBatches(toAdd) {
		args := append([]string{"--literal-pathspecs", "add", "--"}, batch...)
		if _, err := stepGitRun(sctx, args...); err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		sctx.Log(fmt.Sprintf("staging skipped untracked tool scratch: %s (add a .gitignore entry or commit them deliberately)", strings.Join(capList(skipped), ", ")))
	}
	return nil
}

func nulFields(raw string) []string {
	var fields []string
	for _, field := range strings.Split(raw, "\x00") {
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

func appendUnique(list []string, seen map[string]bool, value string) []string {
	if seen[value] {
		return list
	}
	seen[value] = true
	return append(list, value)
}

func pathBatches(paths []string) [][]string {
	var batches [][]string
	var current []string
	size := 0
	for _, p := range paths {
		if len(current) > 0 && size+len(p)+1 > stagingBatchBytes {
			batches = append(batches, current)
			current, size = nil, 0
		}
		current = append(current, p)
		size += len(p) + 1
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// capList returns at most maxListedPaths entries, with a trailing summary of
// how many were left out.
func capList(items []string) []string {
	if len(items) <= maxListedPaths {
		return items
	}
	out := append([]string(nil), items[:maxListedPaths]...)
	return append(out, fmt.Sprintf("... and %d more", len(items)-maxListedPaths))
}

// rawDiffEntry is one record of `git diff --raw -z --no-renames`.
type rawDiffEntry struct {
	status  string
	oldMode string
	newMode string
	newSHA  string
	path    string
}

// parseRawDiff parses the NUL-delimited --raw output of `git diff --cached`
// and `git diff-tree -r`. Renames must be disabled so each record has one path.
func parseRawDiff(raw string) ([]rawDiffEntry, error) {
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var entries []rawDiffEntry
	for i := 0; i < len(fields); i++ {
		if fields[i] == "" {
			continue
		}
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("invalid git raw diff record %q", fields[i])
		}
		i++
		entries = append(entries, rawDiffEntry{
			oldMode: meta[0],
			newMode: meta[1],
			newSHA:  meta[3],
			status:  meta[4],
			path:    fields[i],
		})
	}
	return entries, nil
}

// addedOrModified keeps entries that introduce a regular blob.
func addedOrModified(entries []rawDiffEntry) []rawDiffEntry {
	var out []rawDiffEntry
	for _, entry := range entries {
		if entry.newMode == "160000" || entry.newMode == "000000" || git.IsZeroSHA(entry.newSHA) {
			continue
		}
		switch entry.status[:1] {
		case "A", "M", "T", "C":
			out = append(out, entry)
		}
	}
	return out
}

type oversizedFile struct {
	path string
	size int64
}

// oversizedBlobs returns the entries whose new blob exceeds limit bytes.
func oversizedBlobs(sctx *pipeline.StepContext, entries []rawDiffEntry, limit int64) ([]oversizedFile, error) {
	if limit <= 0 || len(entries) == 0 {
		return nil, nil
	}
	var input strings.Builder
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.newSHA] {
			seen[entry.newSHA] = true
			input.WriteString(entry.newSHA + "\n")
		}
	}
	out, err := stepGitRunInput(sctx, input.String(), "cat-file", "--batch-check")
	if err != nil {
		return nil, fmt.Errorf("measure blob sizes: %w", err)
	}
	sizes := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		var sha, kind string
		var size int64
		if n, _ := fmt.Sscanf(line, "%s %s %d", &sha, &kind, &size); n == 3 && kind == "blob" {
			sizes[sha] = size
		}
	}
	var over []oversizedFile
	for _, entry := range entries {
		if size := sizes[entry.newSHA]; size > limit {
			over = append(over, oversizedFile{path: entry.path, size: size})
		}
	}
	return over, nil
}

func formatOversized(files []oversizedFile) string {
	items := make([]string, 0, len(files))
	for _, f := range files {
		items = append(items, fmt.Sprintf("%s (%s)", f.path, humanBytes(f.size)))
	}
	return strings.Join(capList(items), ", ")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// refuseOversizedStaged refuses the automatic commit when any staged blob
// exceeds max_file_size_bytes, restoring the index to its pre-staging state so
// nothing is partially committed and worktree files are untouched.
func refuseOversizedStaged(sctx *pipeline.StepContext) error {
	limit := sctx.Config.MaxFileSizeBytes
	if limit <= 0 {
		return nil
	}
	raw, err := stepGitRunRaw(sctx, "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check staged file sizes: %w", err)
	}
	entries, err := parseRawDiff(raw)
	if err != nil {
		return fmt.Errorf("check staged file sizes: %w", err)
	}
	over, err := oversizedBlobs(sctx, addedOrModified(entries), limit)
	if err != nil {
		return fmt.Errorf("check staged file sizes: %w", err)
	}
	if len(over) == 0 {
		return nil
	}
	return &pipeline.ProtectedPathError{
		Path: over[0].path,
		Rule: "max_file_size_bytes",
		Detail: fmt.Sprintf("refusing automatic commit: %d staged file(s) exceed the max_file_size_bytes limit of %s: %s; index and worktree preserved. "+
			"Add the files to .gitignore or delete them, or raise max_file_size_bytes (0 disables the check) in the trusted default-branch .no-mistakes.yaml, then retry",
			len(over), humanBytes(limit), formatOversized(over)),
	}
}
