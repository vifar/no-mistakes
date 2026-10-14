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

// GetRequiredCheckContexts returns the status check contexts the target branch
// requires before merging: the union of classic branch protection
// (required_status_checks, both the legacy contexts list and checks[].context)
// and repository rulesets (required_status_checks rules that apply to the
// branch). A source that does not exist or is not readable with the current
// credentials (HTTP 404 or 403) contributes nothing; when neither source is
// readable the result is scm.ErrRequiredChecksUnavailable. Any other failure
// is returned as is.
func (h *Host) GetRequiredCheckContexts(ctx context.Context, _ *scm.PR, branch string) ([]string, error) {
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

	var contexts []string
	readable := false

	protection, ok, err := h.apiGET(ctx, "repos/"+repo+"/branches/"+escaped+"/protection/required_status_checks")
	if err != nil {
		return nil, fmt.Errorf("read branch protection required checks: %w", err)
	}
	if ok {
		var parsed struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(protection, &parsed); err != nil {
			return nil, fmt.Errorf("parse branch protection required checks: %w", err)
		}
		readable = true
		contexts = append(contexts, parsed.Contexts...)
		for _, c := range parsed.Checks {
			contexts = append(contexts, c.Context)
		}
	}

	rules, ok, err := h.apiGET(ctx, "repos/"+repo+"/rules/branches/"+escaped+"?per_page=100")
	if err != nil {
		return nil, fmt.Errorf("read branch rules: %w", err)
	}
	if ok {
		var parsed []struct {
			Type       string `json:"type"`
			Parameters struct {
				RequiredStatusChecks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(rules, &parsed); err != nil {
			return nil, fmt.Errorf("parse branch rules: %w", err)
		}
		readable = true
		for _, rule := range parsed {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, c := range rule.Parameters.RequiredStatusChecks {
				contexts = append(contexts, c.Context)
			}
		}
	}

	if !readable {
		return nil, fmt.Errorf("%w: branch %q has no readable protection or rules", scm.ErrRequiredChecksUnavailable, branch)
	}
	return contexts, nil
}

// apiGET performs a GET through gh and reports ok=false, with no error, when
// the endpoint answers 404 or 403.
func (h *Host) apiGET(ctx context.Context, endpoint string) ([]byte, bool, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "HTTP 404") || strings.Contains(text, "HTTP 403") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("gh api %s: %s: %w", endpoint, strings.TrimSpace(text), err)
	}
	return out, true, nil
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
