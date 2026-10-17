package steps

import (
	"fmt"
	"path"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// docOnlyExtensions are the prose formats a doc-only carry may cover. Plain
// .txt is excluded: files such as requirements.txt configure builds.
var docOnlyExtensions = map[string]bool{".md": true, ".mdx": true, ".markdown": true, ".rst": true, ".adoc": true}

// agentInstructionNames are prose files that steer agents or tools, so editing
// them can change pipeline behavior; they are never documentation here.
var agentInstructionNames = map[string]bool{"agents.md": true, "agent.md": true, "claude.md": true, "gemini.md": true, "skill.md": true}

// isDocOnlyPath applies the built-in documentation rule.
func isDocOnlyPath(p string) bool { return isDocOnlyPathFor(p, nil) }

// isDocOnlyPathFor reports whether a repository path is plain documentation
// for the doc-only carry. Hidden paths, agent instruction files, and anything
// under skills or prompts directories never count, whatever the repository
// configures. Configured globs (document.doc_only_paths) then name the
// documentation; without them only prose at the repository root or under
// docs/ counts. Everything else fails closed.
func isDocOnlyPathFor(p string, configured []string) bool {
	p = strings.TrimPrefix(path.Clean(p), "./")
	if agentInstructionNames[strings.ToLower(path.Base(p))] {
		return false
	}
	segments := strings.Split(p, "/")
	for _, segment := range segments {
		lower := strings.ToLower(segment)
		if strings.HasPrefix(segment, ".") || lower == "skills" || lower == "prompts" {
			return false
		}
	}
	if len(configured) > 0 {
		for _, pattern := range configured {
			if matchIgnorePattern(p, pattern) {
				return true
			}
		}
		return false
	}
	if !docOnlyExtensions[strings.ToLower(path.Ext(p))] {
		return false
	}
	return len(segments) == 1 || segments[0] == "docs"
}

// docOnlyRange reports whether from..to is a descendant range whose diff
// touches only documentation paths. Renames are split into a deletion and an
// addition so both sides are classified.
func docOnlyRange(sctx *pipeline.StepContext, from, to string) (bool, error) {
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", from, to); err != nil {
		return false, nil
	}
	out, err := stepGitRun(sctx, "diff", "--name-only", "--no-renames", "-z", from, to)
	if err != nil {
		return false, fmt.Errorf("list doc-only carry paths: %w", err)
	}
	paths := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	if len(paths) == 0 || paths[0] == "" {
		return false, nil
	}
	var configured []string
	if sctx.Config != nil {
		configured = sctx.Config.Document.DocOnlyPaths
	}
	for _, p := range paths {
		if !isDocOnlyPathFor(p, configured) {
			return false, nil
		}
	}
	return true, nil
}

// reviewRestartNeededForPush decides whether Push must send the run back to
// Review before publishing headBeingPushed. A head Push itself committed, or
// one the review approval does not name, needs Review again - except when the
// only difference from the approved head is documentation (typically the
// Document step's own commit). Then Review and Test credit carry onto the
// pushed head, so a Document commit no longer restarts validation and the run
// converges instead of churning one generation per Document pass. Strict
// shipping keeps its exact-head approval and never carries.
func reviewRestartNeededForPush(sctx *pipeline.StepContext, newHeadSHA, headBeingPushed string) (bool, error) {
	if newHeadSHA != "" {
		return true, nil
	}
	approved, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return false, err
	}
	if approved.ReviewApprovedHeadSHA == nil || *approved.ReviewApprovedHeadSHA == headBeingPushed {
		return false, nil
	}
	if sctx.Run.Shipping != nil || approved.HeadSHA != headBeingPushed {
		return true, nil
	}
	from := *approved.ReviewApprovedHeadSHA
	docOnly, err := docOnlyRange(sctx, from, headBeingPushed)
	if err != nil || !docOnly {
		return true, err
	}
	carried, err := sctx.DB.CarryValidationOverDocOnlyCommits(sctx.Run.ID, from, headBeingPushed)
	if err != nil || !carried {
		return true, err
	}
	sctx.Run.ReviewApprovedHeadSHA = &headBeingPushed
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("documentation-only commits since the review-approved head %s; carrying Review and Test credit to %s", shortObjectID(from), shortObjectID(headBeingPushed)))
	}
	return false, nil
}
