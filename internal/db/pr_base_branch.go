package db

import "strings"

// LineagePRBaseBranch selects the first non-empty base recorded by an ordinary
// run on branch. Callers supply one repository's runs newest first. Strict
// shipping records carry their own pinned target and must never change the
// ordinary lineage's base. Launch inheritance and recovery handoffs share this
// selection so they cannot disagree about the lineage's recorded base.
func LineagePRBaseBranch(runs []*Run, branch string) string {
	for _, run := range runs {
		if run.Branch != branch || run.Shipping != nil || run.PRBaseBranch == nil {
			continue
		}
		if base := strings.TrimSpace(*run.PRBaseBranch); base != "" {
			return base
		}
	}
	return ""
}
