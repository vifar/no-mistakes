package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
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
	switch {
	case err != nil && classicProtectionAbsent(err):
		// A branch governed only by rulesets has no classic protection (or none
		// requiring checks); that source is empty, and the rules read below
		// still decides. A bare Not Found can also hide protection from a
		// caller without admin access, so it stays unavailable.
		protection = []byte(`{}`)
	case err != nil:
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
		var pages [][]rule
		if err := json.Unmarshal(rules, &pages); err != nil {
			return nil, fmt.Errorf("parse branch rules: %w", err)
		}
		var all []rule
		for _, page := range pages {
			all = append(all, page...)
		}
		// required_status_checks is the only check requirement modelled here.
		// These rule types also gate merges on checks or statuses but name no
		// check context (a required workflow's job names are not in the
		// rule), so a required set derived without them would be incomplete:
		// treat the set as unavailable and let the caller judge every check.
		var unmodeled []string
		for _, r := range all {
			if unmodeledCheckRules[r.Type] && !slices.Contains(unmodeled, r.Type) {
				unmodeled = append(unmodeled, r.Type)
			}
		}
		if len(unmodeled) > 0 {
			slices.Sort(unmodeled)
			return nil, fmt.Errorf("%w: branch %q has %s rule(s) whose checks cannot be mapped to check runs", scm.ErrRequiredChecksUnavailable, branch, strings.Join(unmodeled, ", "))
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

	for i := range required {
		required[i].SelfStatus = strings.TrimSpace(required[i].Context) == CommitStatusContext
	}
	return required, nil
}

// unmodeledCheckRules are the ruleset rule types that gate merging on checks,
// statuses, or deployments without naming check contexts.
var unmodeledCheckRules = map[string]bool{
	"workflows":            true,
	"code_scanning":        true,
	"required_deployments": true,
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

// classicProtectionAbsent reports whether a classic protection read failed
// because the branch has no protection or none requiring status checks, as
// opposed to a permission or lookup failure that GitHub also reports as 404.
func classicProtectionAbsent(err error) bool {
	text := err.Error()
	return strings.Contains(text, "HTTP 404") &&
		(strings.Contains(text, "Branch not protected") || strings.Contains(text, "Required status checks not enabled"))
}

// escapeBranchPath percent-encodes a branch name as one REST path parameter.
// Slashes become %2F: GitHub treats {branch} as a single segment, and gh api
// sends the endpoint as given, so release/1.x must not split into two segments.
func escapeBranchPath(branch string) string {
	return url.PathEscape(branch)
}
