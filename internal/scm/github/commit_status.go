package github

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"strings"
)

// CommitStatusContext names the plain GitHub status published by the pipeline.
const CommitStatusContext = "no-mistakes"

// PostCommitStatus uses the same gh command factory and host-scoped credentials
// as PR and check operations. The caller must supply its exact published SHA.
func (h *Host) PostCommitStatus(ctx context.Context, sha, state, description, targetURL string) error {
	if strings.TrimSpace(sha) == "" || h.repoSlug() == "" {
		return fmt.Errorf("commit status requires a repository and published SHA")
	}
	args := []string{"api", "--method", "POST"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "repos/"+h.repoSlug()+"/statuses/"+sha, "-f", "state="+state, "-f", "context="+CommitStatusContext, "-f", "description="+description)
	if targetURL != "" {
		args = append(args, "-f", "target_url="+targetURL)
	}
	cmd := h.cmd(ctx, "gh", args...)
	shellenv.ConfigureShellCommand(cmd)
	out, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		return fmt.Errorf("post GitHub commit status: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
