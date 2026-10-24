package github

import (
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func ghCommandWasStopped(exit *exec.ExitError) bool {
	return exit != nil && uint32(exit.ExitCode()) == shellenv.CommandCancelledExitCode
}
