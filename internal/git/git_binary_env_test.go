package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/execpath"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// A command that is handed an environment uses THAT environment, for the
// binary as well as for the child.
//
// exec.CommandContext resolves a bare name against the PROCESS's PATH, so an
// environment that carries its own PATH used to be honoured for the child's
// environment and ignored for the binary lookup. A step's environment is
// exactly such an environment - it carries the step's scoped credentials and
// the binaries the step injects - so the command ran a different git than the
// step asked for.
func TestRunWithEnvUsesTheEnvironmentItIsGiven(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	writeEnvironmentGit(t, binDir)

	out, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + binDir}, "version")
	if err != nil {
		t.Fatalf("git with the given environment: %v", err)
	}
	if !strings.Contains(out, "the-environment-git") {
		t.Fatalf("the command ran a git the given environment does not name: %q", out)
	}

	// An environment with no PATH of its own keeps the process's PATH, so
	// every caller that does not choose one is unaffected.
	real, err := Run(context.Background(), dir, "version")
	if err != nil {
		t.Fatalf("git with the process environment: %v", err)
	}
	if strings.Contains(real, "the-environment-git") {
		t.Fatalf("the command used an environment it was not given: %q", real)
	}
}

// An environment whose PATH names no git says so, rather than quietly running
// a different binary than the caller asked for.
func TestRunWithEnvRefusesAnEnvironmentWithoutGit(t *testing.T) {
	dir := t.TempDir()
	empty := t.TempDir()
	_, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + empty}, "version")
	if err == nil {
		t.Fatal("a PATH with no git in it reported success")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Fatalf("the refusal does not name the binary: %v", err)
	}
}

func writeEnvironmentGit(t *testing.T, binDir string) {
	t.Helper()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		source := filepath.Join(t.TempDir(), "main.go")
		if err := os.WriteFile(source, []byte(`package main
import ("fmt"; "os")
func main() { fmt.Print("the-environment-git:" + os.Getenv("PATH")) }
`), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "git.exe"), source)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fixture: %v: %s", err, out)
		}
		return
	}
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\nprintf 'the-environment-git:%s' \"$PATH\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunWithEnvPathResolution(t *testing.T) {
	for _, relative := range []string{"bin", "."} {
		t.Run(relative, func(t *testing.T) {
			dir := t.TempDir()
			writeEnvironmentGit(t, filepath.Join(dir, relative))
			path := relative + string(os.PathListSeparator) + os.Getenv("PATH")
			out, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + path}, "version")
			if err != nil || out != "the-environment-git:"+path {
				t.Fatalf("relative PATH: output %q, error %v", out, err)
			}
		})
	}
	dir := t.TempDir()
	bin := t.TempDir()
	writeEnvironmentGit(t, bin)
	path := "PATH=" + bin
	if runtime.GOOS == "windows" {
		path = "Path=" + bin
	}
	out, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + t.TempDir(), path}, "version")
	if err != nil || out != "the-environment-git:"+bin {
		t.Fatalf("last PATH override: output %q, error %v", out, err)
	}
	ctx := WithEnvironment(context.Background(), runenv.Overlay{Set: map[string]string{"PATH": bin}})
	out, err = Run(ctx, dir, "version")
	if err != nil || out != "the-environment-git:"+bin {
		t.Fatalf("context PATH: output %q, error %v", out, err)
	}
}

func TestRunWithEnvEmptyPathDoesNotInherit(t *testing.T) {
	_, err := RunWithEnv(context.Background(), t.TempDir(), []string{"PATH="}, "version")
	if err == nil {
		t.Fatal("empty PATH used process Git")
	}
}

func TestEnvValueForOSLastOverride(t *testing.T) {
	for _, tc := range []struct{ goos, want string }{{"windows", "third"}, {"linux", "second"}} {
		got, ok := execpath.ValueForOS([]string{"PATH=first", "PATH=second", "Path=third"}, "PATH", tc.goos)
		if !ok || got != tc.want {
			t.Fatalf("%s PATH = %q, %v; want %q", tc.goos, got, ok, tc.want)
		}
	}
}
