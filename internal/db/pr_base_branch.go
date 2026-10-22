package db

import "strings"

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
