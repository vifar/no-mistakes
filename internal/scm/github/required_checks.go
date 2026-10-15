package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var _ scm.RequiredChecksReader = (*Host)(nil)

// GetRequiredChecks returns the status checks the target branch requires,
// unioned across classic branch protection and every page of the branch's
// rules. A requirement bound to an application (protection app_id, ruleset
// integration_id) keeps that identity; -1, null, or absent means any producer.
func (h *Host) GetRequiredChecks(ctx context.Context, _ *scm.PR, branch string) ([]scm.RequiredCheck, error) {
	repo := h.repoSlug()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("resolve GitHub repository for required checks: invalid repository %q", repo)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, fmt.Errorf("%w: no target branch", scm.ErrRequiredChecksUnavailable)
	}
	escaped := escapeBranchPath(branch)

	var required []scm.RequiredCheck

	protection, err := h.apiGET(ctx, "repos/"+repo+"/branches/"+escaped+"/protection/required_status_checks")
	if err != nil {
		return nil, fmt.Errorf("read branch protection required checks: %w", err)
	}
	{
		var parsed struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
				AppID   *int64 `json:"app_id"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(protection, &parsed); err != nil {
			return nil, fmt.Errorf("parse branch protection required checks: %w", err)
		}
		bound := make(map[string]bool, len(parsed.Checks))
		for _, c := range parsed.Checks {
			bound[c.Context] = true
			required = append(required, scm.RequiredCheck{Context: c.Context, AppID: appIDOrZero(c.AppID)})
		}
		// The legacy contexts list repeats every entry of checks without its
		// application; only contexts checks does not name are unbound.
		for _, c := range parsed.Contexts {
			if !bound[c] {
				required = append(required, scm.RequiredCheck{Context: c})
			}
		}
	}

	rules, err := h.apiGET(ctx, "repos/"+repo+"/rules/branches/"+escaped+"?per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("read branch rules: %w", err)
	}
	{
		type rule struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredStatusChecks []struct {
					Context       string `json:"context"`
					IntegrationID *int64 `json:"integration_id"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		// --slurp wraps each page in an outer array; flatten every page before
		// deriving contexts, since the required_status_checks rule can be on
		// any of them.
		var pages []json.RawMessage
		if err := json.Unmarshal(rules, &pages); err != nil {
			return nil, fmt.Errorf("parse branch rules: %w", err)
		}
		var all []rule
		for _, raw := range pages {
			trimmed := strings.TrimSpace(string(raw))
			if strings.HasPrefix(trimmed, "[") {
				var page []rule
				if err := json.Unmarshal(raw, &page); err != nil {
					return nil, fmt.Errorf("parse branch rules: %w", err)
				}
				all = append(all, page...)
				continue
			}
			var single rule
			if err := json.Unmarshal(raw, &single); err != nil {
				return nil, fmt.Errorf("parse branch rules: %w", err)
			}
			all = append(all, single)
		}
		for _, r := range all {
			if r.Type != "required_status_checks" {
				continue
			}
			for _, c := range r.Parameters.RequiredStatusChecks {
				required = append(required, scm.RequiredCheck{Context: c.Context, AppID: appIDOrZero(c.IntegrationID)})
			}
		}
	}

	return required, nil
}

func appIDOrZero(id *int64) int64 {
	if id == nil || *id < 0 {
		return 0
	}
	return *id
}

func (h *Host) apiGET(ctx context.Context, endpoint string, extra ...string) ([]byte, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint)
	args = append(args, extra...)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "HTTP 404") || strings.Contains(text, "HTTP 403") {
			return nil, fmt.Errorf("%w: %s", scm.ErrRequiredChecksUnavailable, strings.TrimSpace(text))
		}
		return nil, fmt.Errorf("gh api %s: %s: %w", endpoint, strings.TrimSpace(text), err)
	}
	return out, nil
}

// escapeBranchPath percent-encodes each segment of a branch name for use in a
// REST path while keeping the separators between segments.
func escapeBranchPath(branch string) string {
	segments := strings.Split(branch, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
