//go:build windows

package steps

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func lintCheckCommand(ctx context.Context, binary string, args []string) (*exec.Cmd, error) {
	ext := strings.ToLower(filepath.Ext(binary))
	if ext != ".cmd" && ext != ".bat" {
		return exec.CommandContext(ctx, binary, args...), nil
	}
	line, err := lintWindowsShimCommandLine(binary, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	return cmd, nil
}
