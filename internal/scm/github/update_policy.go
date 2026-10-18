package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// RequiresUpToDate unions classic protection and every applicable rules page.
// Missing fields, permission errors and unknown check rules fail closed.
func (h *Host) RequiresUpToDate(ctx context.Context, _ *scm.PR, branch string) (bool, error) {
	if strings.TrimSpace(branch) == "" {
		return false, fmt.Errorf("empty protection branch")
	}
	repo := h.repoSlug()
	if len(strings.Split(repo, "/")) != 2 {
		return false, fmt.Errorf("invalid protection repository")
	}
	escaped := escapeBranchPath(branch)
	protection, err := h.apiGET(ctx, "repos/"+repo+"/branches/"+escaped+"/protection/required_status_checks")
	absent := err != nil && classicProtectionAbsent(err)
	if err != nil && !absent {
		return false, fmt.Errorf("read up-to-date protection: %w", err)
	}
	required := false
	if !absent {
		var p struct {
			Strict *bool `json:"strict"`
		}
		if err := json.Unmarshal(protection, &p); err != nil {
			return false, err
		}
		if p.Strict == nil {
			return false, fmt.Errorf("branch protection omitted strict policy")
		}
		required = *p.Strict
	}
	rules, err := h.apiGET(ctx, "repos/"+repo+"/rules/branches/"+escaped+"?per_page=100", "--paginate", "--slurp")
	if err != nil {
		return false, fmt.Errorf("read up-to-date rules: %w", err)
	}
	var pages [][]struct {
		Type       string `json:"type"`
		Parameters struct {
			Strict *bool `json:"strict_required_status_checks_policy"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(rules, &pages); err != nil {
		return false, err
	}
	if pages == nil {
		return false, fmt.Errorf("branch rules omitted pages")
	}
	for _, page := range pages {
		for _, rule := range page {
			if rule.Type == "merge_queue" {
				required = true
			}
			if rule.Type == "required_status_checks" {
				if rule.Parameters.Strict == nil {
					return false, fmt.Errorf("required checks rule omitted strict policy")
				}
				required = required || *rule.Parameters.Strict
			} else if unmodeledCheckRules[rule.Type] {
				return false, fmt.Errorf("unmodeled up-to-date rule %s", rule.Type)
			}
		}
	}
	return required, nil
}

var _ scm.BranchUpdatePolicyReader = (*Host)(nil)
