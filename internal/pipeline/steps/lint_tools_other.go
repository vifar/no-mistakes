//go:build !windows

package steps

import (
	"context"
	"os/exec"
)

func lintCheckCommand(ctx context.Context, binary string, args []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, binary, args...), nil
}
