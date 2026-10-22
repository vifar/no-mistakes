package daemon

import "github.com/kunchenguid/no-mistakes/internal/db"

// The base branch is part of what the review is about. A pull request that
// targets develop and a pull request that targets main are different review
// objects, with different scope, different comparison, and different merge
// destination. A run that continues a lineage therefore inherits the
// lineage's base branch instead of resolving the repository default again.

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
	return db.LineagePRBaseBranch(runs, branch), nil
}
