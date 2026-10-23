//go:build !windows

package github

import (
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func ghCommandWasStopped(exit *exec.ExitError) bool { return git.ProcessWasCancelled(exit) }
