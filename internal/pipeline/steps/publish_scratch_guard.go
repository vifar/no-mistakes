package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// isAncestorCommit reports whether ancestor names a commit reachable from
// descendant (or equal to it).
func isAncestorCommit(sctx *pipeline.StepContext, ancestor, descendant string) bool {
	if ancestor == "" || git.IsZeroSHA(ancestor) {
		return false
	}
	if _, err := stepGitRun(sctx, "cat-file", "-e", ancestor+"^{commit}"); err != nil {
		return false
	}
	_, err := stepGitRun(sctx, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// resolvePublicationBase is the single place the publication guard chooses the
// commit it scans from. Branch refs and remote-tracking refs live in the
// worktree an agent can write, so none of them is trusted by itself:
//
//   - Pinned evidence is what the run recorded outside the worktree: the PR
//     comparison receipt's merge-base, else the run's BaseSHA. It counts only
//     when it is an ancestor of the head being published.
//   - The merge-base with the PR target is derived from a fresh fetch of the
//     target into a private ref, never from refs/remotes/origin/* or a local
//     branch, so a moved ref cannot shrink the range. It narrows the pinned base
//     (it may be newer, after the target advanced) but must itself contain it.
//
// When neither source yields a base it returns an error: scanning nothing and
// calling the range clean would defeat the guard.
func resolvePublicationBase(sctx *pipeline.StepContext, head string) (string, error) {
	pinned := ""
	if rc := sctx.PRContext; rc != nil && isAncestorCommit(sctx, rc.MergeBaseSHA, head) {
		pinned = rc.MergeBaseSHA
	} else if isAncestorCommit(sctx, strings.TrimSpace(sctx.Run.BaseSHA), head) {
		pinned = strings.TrimSpace(sctx.Run.BaseSHA)
	}

	target := effectivePRBaseBranch(sctx)
	var fetchErr error
	if sctx.EvalReplay {
		// Eval replay pins origin/<base> in an isolated worktree with no upstream.
		if mb, err := stepGitRun(sctx, "merge-base", head, "origin/"+target); err == nil && mb != "" {
			if pinned == "" || isAncestorCommit(sctx, pinned, mb) {
				return mb, nil
			}
		}
	} else {
		private := "refs/no-mistakes/publication-base/" + sctx.Run.ID
		ctx, cancel := context.WithTimeoutCause(sctx.Ctx, fetchUpstreamTimeout, ErrFetchTimeout)
		fetchErr = git.FetchRemoteBranchToPrivateRef(ctx, sctx.WorkDir, resolveUpstreamURL(sctx), target, private)
		if fetchErr != nil && errors.Is(context.Cause(ctx), ErrFetchTimeout) {
			fetchErr = fmt.Errorf("%w after %s: %v", ErrFetchTimeout, fetchUpstreamTimeout, fetchErr)
		}
		cancel()
		if fetchErr == nil {
			defer func() { _, _ = stepGitRun(sctx, "update-ref", "--no-deref", "-d", private) }()
			if mb, err := stepGitRun(sctx, "merge-base", head, private); err == nil && mb != "" {
				if pinned == "" || isAncestorCommit(sctx, pinned, mb) {
					return mb, nil
				}
			}
		}
	}
	if pinned != "" {
		return pinned, nil
	}
	detail := "no recorded base is an ancestor of the head"
	if fetchErr != nil {
		detail += fmt.Sprintf(" and fetching %q failed: %v", target, fetchErr)
	}
	return "", fmt.Errorf("refusing to publish %s: cannot establish a trustworthy base to scan for oversized files and tool scratch (%s)", shortSHA(head), detail)
}

// assertPublishedRangeClean refuses to publish a range that adds or modifies a
// blob over max_file_size_bytes, or adds a path under a new scratch directory
// or matching staging_exclude. It covers agent commits with `git add -A`,
// which bypass scratch-safe staging, and fails before anything leaves the
// machine rather than at a forge's push limit.
func assertPublishedRangeClean(sctx *pipeline.StepContext, head string) error {
	limit := sctx.Config.MaxFileSizeBytes
	base, err := resolvePublicationBase(sctx, head)
	if err != nil {
		return err
	}
	baseObjects, err := publicationTreeObjects(sctx, base)
	if err != nil {
		return err
	}
	commits, err := stepGitRun(sctx, "rev-list", "--reverse", "--parents", base+".."+head)
	if err != nil {
		return fmt.Errorf("scan published commits: %w", err)
	}
	var entries []rawDiffEntry
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
		inherited := []map[string]string{baseObjects}
		if len(fields) > 2 {
			for _, parent := range fields[1:] {
				objects, err := publicationTreeObjects(sctx, parent)
				if err != nil {
					return err
				}
				inherited = append(inherited, objects)
			}
		}
		for _, entry := range changes {
			introduced := true
			for _, objects := range inherited {
				if objects[entry.path] == entry.newSHA {
					introduced = false
					break
				}
			}
			if introduced {
				entries = append(entries, entry)
			}
		}
	}

	var scratch []string
	addedPathsSeen := map[string]bool{}
	baseDirs := map[string]bool{}
	dirInBase := func(dir string) (bool, error) {
		if known, ok := baseDirs[dir]; ok {
			return known, nil
		}
		exists, err := gitTreeHasDirectory(sctx, base, dir)
		if err != nil {
			return false, err
		}
		baseDirs[dir] = exists
		return exists, nil
	}
	for _, entry := range entries {
		// Gitlinks are exempt from blob measurement only; their paths still
		// count as scratch.
		if entry.status != "A" || addedPathsSeen[entry.path] {
			continue
		}
		addedPathsSeen[entry.path] = true
		if pattern, excluded := matchesStagingExclude(sctx, entry.path); excluded {
			scratch = append(scratch, fmt.Sprintf("%s (matches staging_exclude %q)", entry.path, pattern))
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
				scratch = append(scratch, fmt.Sprintf("%s (new directory %s/)", entry.path, dir))
				break
			}
		}
	}
	over, err := oversizedBlobs(sctx, addedOrModified(entries), limit)
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

func publicationTreeObjects(sctx *pipeline.StepContext, tree string) (map[string]string, error) {
	raw, err := stepGitRunRaw(sctx, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, fmt.Errorf("scan publication tree %s: %w", tree, err)
	}
	objects := map[string]string{}
	for _, record := range nulFields(raw) {
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid publication tree record %q", record)
		}
		objects[path] = fields[2]
	}
	return objects, nil
}
