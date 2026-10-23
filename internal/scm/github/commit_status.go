package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
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
	_, err := netretry.Do(ctx, h.retryOptions("post GitHub commit status"), func() (struct{}, error) {
		cmd := h.cmd(ctx, "gh", args...)
		shellenv.ConfigureShellCommand(cmd)
		out, callErr := shellenv.CombinedOutputShellCommand(cmd)
		if callErr != nil {
			return struct{}{}, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), callErr)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("post GitHub commit status: %w", err)
	}
	return nil
}
