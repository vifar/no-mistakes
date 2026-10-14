package gatecontext_test

import (
	"github.com/kunchenguid/no-mistakes/internal/testgit/gitfault"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Fixtures must not depend on the machine's system git config: some
	// distributions ship safe.bareRepository=explicit there, which refuses
	// the in-directory use of the bare fixture repositories these tests
	// create. Mirrors internal/git's TestMain.
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Fixtures must not inherit the developer's global git config: a
	// commit.gpgsign=true there makes every fixture commit ask gpg to sign.
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-gatecontext-tests-")
	if err != nil {
		panic(err)
	}
	globalConfig := filepath.Join(globalConfigDir, "gitconfig")
	os.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	if err := os.Unsetenv("GIT_CONFIG_COUNT"); err != nil {
		panic(err)
	}
	code := m.Run()
	gitfault.Cleanup()
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}
