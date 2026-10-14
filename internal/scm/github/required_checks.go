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

	protection, err := h.apiGET(ctx, "repos/"+repo+"/branches/"+escaped+"/protection/required_status_checks")
	if err != nil {
		return nil, fmt.Errorf("read branch protection required checks: %w", err)
	}
	{
		var parsed struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(protection, &parsed); err != nil {
			return nil, fmt.Errorf("parse branch protection required checks: %w", err)
		}
		contexts = append(contexts, parsed.Contexts...)
		for _, c := range parsed.Checks {
			contexts = append(contexts, c.Context)
		}
	}

	rules, err := h.apiGET(ctx, "repos/"+repo+"/rules/branches/"+escaped+"?per_page=100")
	if err != nil {
		return nil, fmt.Errorf("read branch rules: %w", err)
	}
	{
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
		for _, rule := range parsed {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, c := range rule.Parameters.RequiredStatusChecks {
				contexts = append(contexts, c.Context)
			}
		}
	}

	return contexts, nil
}

func (h *Host) apiGET(ctx context.Context, endpoint string) ([]byte, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint)
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
