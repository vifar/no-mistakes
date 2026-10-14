package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// publicationScanBase picks the commit a publication guard diffs against: the
// merge-base with the local copy of the PR target (no network), else the run's
// recorded base. It returns "" when neither is usable.
func publicationScanBase(sctx *pipeline.StepContext, head string) string {
	target := effectivePRBaseBranch(sctx)
	for _, ref := range []string{"origin/" + target, target} {
		if mb, err := stepGitRun(sctx, "merge-base", head, ref); err == nil && mb != "" {
			return mb
		}
	}
	if base := strings.TrimSpace(sctx.Run.BaseSHA); base != "" && !git.IsZeroSHA(base) {
		if _, err := stepGitRun(sctx, "cat-file", "-e", base+"^{commit}"); err == nil {
			return base
		}
	}
	return ""
}

// assertPublishedRangeClean refuses to publish a range that adds a blob over
// max_file_size_bytes, a path under a new scratch directory, or a path matching
// staging_exclude. It covers commits an agent made itself with `git add -A`,
// which bypass scratch-safe staging, and fails before anything leaves the
// machine rather than at a forge's push limit.
func assertPublishedRangeClean(sctx *pipeline.StepContext, head string) error {
	limit := sctx.Config.MaxFileSizeBytes
	base := publicationScanBase(sctx, head)
	if base == "" {
		sctx.Log("warning: no usable base to scan the published range for oversized files and tool scratch; skipping the check")
		return nil
	}
	commits, err := stepGitRun(sctx, "rev-list", "--reverse", "--parents", base+".."+head)
	if err != nil {
		return fmt.Errorf("scan published commits: %w", err)
	}
	var entries, introduced []rawDiffEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(commits, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		args := []string{"diff-tree", "-r", "-z", "--root", "--no-commit-id", "--no-abbrev", "--no-renames"}
		if len(fields) > 1 {
			args = append(args, fields[1])
		}
		args = append(args, fields[0])
		raw, err := stepGitRunRaw(sctx, args...)
		if err != nil {
			return fmt.Errorf("scan published commit %s: %w", fields[0], err)
		}
		changes, err := parseRawDiff(raw)
		if err != nil {
			return fmt.Errorf("scan published commit %s: %w", fields[0], err)
		}
		entries = append(entries, changes...)
		for _, entry := range addedOrModified(changes) {
			key := entry.path + "\x00" + entry.newSHA
			if !seen[key] {
				seen[key] = true
				introduced = append(introduced, entry)
			}
		}
	}

	var scratch []string
	baseDirs := map[string]bool{}
	dirInBase := func(dir string) (bool, error) {
		if known, ok := baseDirs[dir]; ok {
			return known, nil
		}
		out, err := stepGitRun(sctx, "--literal-pathspecs", "ls-tree", "--name-only", base, "--", dir)
		if err != nil {
			return false, err
		}
		baseDirs[dir] = out != ""
		return out != "", nil
	}
	for _, entry := range entries {
		if entry.status != "A" || entry.newMode == "160000" {
			continue
		}
		if pattern, excluded := matchesStagingExclude(sctx, entry.path); excluded {
			scratch = appendUnique(scratch, fmt.Sprintf("%s (matches staging_exclude %q)", entry.path, pattern))
			continue
		}
		// Walk scratch components shallow to deep; the first one the base tree
		// lacks makes the whole path new scratch.
		parts := strings.Split(entry.path, "/")
		for i := 0; i < len(parts)-1; i++ {
			if !isScratchDirName(parts[i]) {
				continue
			}
			dir := strings.Join(parts[:i+1], "/")
			exists, err := dirInBase(dir)
			if err != nil {
				return fmt.Errorf("scan published range: %w", err)
			}
			if !exists {
				scratch = appendUnique(scratch, fmt.Sprintf("%s (new directory %s/)", entry.path, dir))
				break
			}
		}
	}
	over, err := oversizedBlobs(sctx, introduced, limit)
	if err != nil {
		return fmt.Errorf("scan published range: %w", err)
	}
	if len(scratch) == 0 && len(over) == 0 {
		return nil
	}
	var problems []string
	if len(over) > 0 {
		problems = append(problems, fmt.Sprintf("%d file(s) over the max_file_size_bytes limit of %s: %s", len(over), humanBytes(limit), formatOversized(over)))
	}
	if len(scratch) > 0 {
		problems = append(problems, fmt.Sprintf("%d path(s) look like tool scratch: %s", len(scratch), strings.Join(capList(scratch), ", ")))
	}
	return fmt.Errorf("refusing to publish %s: the branch adds %s. "+
		"Remove the content from branch history by rewriting or dropping the offending commits, then retry publication; adding .gitignore alone cannot remove committed content. "+
		"to publish a hidden directory deliberately, commit it before the run so it exists in the base",
		shortSHA(head), strings.Join(problems, "; "))
}
