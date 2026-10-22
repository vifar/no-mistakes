package daemon

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// The base branch is part of what the review is about. A pull request that
// targets develop and a pull request that targets main are different review
// objects, with different scope, different comparison, and different merge
// destination. A run that continues a lineage therefore inherits the
// lineage's base branch instead of resolving the repository default again.

// recordedPRBaseBranch returns the base branch a run recorded for its
// lineage, or "" when the run recorded none.
//
// Only the per-run override is a lineage base branch. A strict shipping run
// persists its own pinned target at launch and resolves its base from that
// pin, so the pin is not inherited as a lineage value.
func recordedPRBaseBranch(run *db.Run) string {
	if run == nil || run.PRBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*run.PRBaseBranch)
}

// lineagePRBaseBranch returns the base branch established by the branch's
// lineage, or "" when the branch has no lineage base branch.
//
// The lookup walks the branch's runs newest first and takes the first run
// that recorded a base branch, so a lineage that established a base branch
// keeps it even when a later member of the same lineage recorded none. A
// member that requested a new base branch moves the lineage onto it, and the
// members that follow inherit the new base.
func (m *RunManager) lineagePRBaseBranch(repoID, branch string) (string, error) {
	if repoID == "" || branch == "" {
		return "", nil
	}
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if base := recordedPRBaseBranch(run); base != "" {
			return base, nil
		}
	}
	return "", nil
}
