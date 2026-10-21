package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestMain(m *testing.M) {
	if handled, err := git.HandleAttributionHook(os.Args[1:]); handled {
		os.Exit(git.AttributionHookExitCode(err))
	}
	testgit.ClearConfigEnv()
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-pipeline-tests-")
	if err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(globalConfigDir, "gitconfig"))
	code := m.Run()
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}
