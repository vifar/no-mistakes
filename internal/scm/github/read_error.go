package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// gh's credential-store diagnostic is a timeout even though gh exits 1, just
// as it does for rejected credentials. Do not classify arbitrary text containing
// "timeout" (for example a PR title) or a real login rejection as retryable.
func isKeyringTimeout(detail string) bool {
	lower := strings.ToLower(detail)
	for _, rejected := range []string{"bad credentials", "not logged in", "failed to log in", "token is invalid"} {
		if strings.Contains(lower, rejected) {
			return false
		}
	}
	for _, line := range strings.Split(lower, "\n") {
		if strings.Contains(line, "timeout trying to log in to ") && strings.Contains(line, " (keyring)") {
			return true
		}
	}
	return false
}

func prReadError(ctx context.Context, operation, detail string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s interrupted: %w", operation, errors.Join(err, ctx.Err()))
	}
	if isKeyringTimeout(detail) {
		return fmt.Errorf("%s: %w: %s: %w", operation, scm.ErrTransientTimeout, strings.TrimSpace(detail), err)
	}
	return fmt.Errorf("%s: %s: %w", operation, detail, err)
}
