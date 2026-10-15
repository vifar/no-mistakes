// Package gitfault provides a separately compiled, non-race Git fault helper
// for process-boundary tests. Production code never imports it.
package gitfault

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

var buildOnce sync.Once
var buildDir, binary string
var buildErr error

func Build(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "nm-git-fault-")
		if buildErr != nil {
			return
		}
		binary = filepath.Join(buildDir, "git")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		_, source, _, _ := runtime.Caller(0)
		cmd := exec.Command("go", "build", "-o", binary, filepath.Join(filepath.Dir(source), "testdata"))
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildFailure{err, string(output)}
		}
		if buildErr == nil {
			binary, buildErr = filepath.EvalSymlinks(binary)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

type buildFailure struct {
	err    error
	output string
}

func (e *buildFailure) Error() string { return e.err.Error() + ": " + e.output }

// Cleanup belongs to the importing package's TestMain, after all helper users.
func Cleanup() {
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
}

func Install(t *testing.T, mode string) string {
	t.Helper()
	helper := Build(t)
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(helper)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NM_GIT_FAULT_REAL", realGit)
	t.Setenv("NM_GIT_FAULT_MODE", mode)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("NM_GIT_FAULT_LOG", log)
	return log
}

// EscapedHolder records the fixture token before spawning. Cleanup verifies
// the exact helper and unique token before killing only that fixture process.
func EscapedHolder(t *testing.T) {
	t.Helper()
	token := t.TempDir()
	ready := filepath.Join(t.TempDir(), "holder")
	t.Setenv("NM_GIT_FAULT_TOKEN", token)
	t.Setenv("NM_GIT_FAULT_READY", ready)
	helper := Build(t)
	t.Cleanup(func() {
		data, err := os.ReadFile(ready)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		fields := strings.Fields(string(data))
		if len(fields) != 2 || fields[1] != token {
			t.Errorf("invalid fixture identity: %q", data)
			return
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == os.Getpid() {
			t.Errorf("invalid fixture pid: %q", data)
			return
		}
		output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err != nil {
			return
		} // A fixture that already exited needs no cleanup.
		command := strings.TrimSpace(string(output))
		if !strings.Contains(command, helper+" hold "+token) {
			t.Errorf("fixture identity changed: %s", command)
			return
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Error(err)
			return
		}
		if err := process.Kill(); err != nil {
			t.Error(err)
		}
	})
}
