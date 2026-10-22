package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/execpath"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

// Repository discovery follows the documented command-directory PATH contract.
// A real repository supplies the expected root independently of the resolver;
// resolving relative entries from the caller's directory must not pass.
func TestFindGitRootUsesCommandDirectoryPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink fixture and empty PATH entry semantics")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		t.Fatal(err)
	}
	repo := initTestRepo(t)
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(repo, "bin")
	sub := filepath.Join(repo, "sub")
	for _, dir := range []string{bin, sub} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realGit, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name, dir, path string
		wantError       bool
	}{
		{"absolute", sub, bin, false},
		{"relative", sub, filepath.Join("..", "bin"), false},
		{"relative child", repo, "bin", false},
		{"current directory", bin, ".", false},
		{"empty entry", bin, string(os.PathListSeparator) + sub, false},
		{"empty PATH", sub, "", true},
		{"missing Git", sub, sub, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			got, err := FindGitRoot(tc.dir)
			if tc.wantError {
				if err == nil {
					t.Fatalf("discovery used a Git outside PATH: %q", got)
				}
				return
			}
			if err != nil || got != want {
				t.Fatalf("FindGitRoot(%q) = %q, %v; want %q", tc.dir, got, err, want)
			}
		})
	}
}

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
	t.Chdir(dir)
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("repo", 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nprintf 'the-environment-git'\n"
	if runtime.GOOS == "windows" {
		fake += ".exe"
		source := filepath.Join(dir, "main.go")
		if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Print(\"the-environment-git\") }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("go", "build", "-o", fake, source).CombinedOutput(); err != nil {
			t.Fatalf("build git fixture: %v\n%s", err, out)
		}
	} else {
		if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name, workDir, path string
	}{
		{"absolute", dir, binDir},
		{"current", ".", "bin"},
		{"relative", "repo", filepath.Join("..", "bin")},
		{"empty", "", "bin"},
		{"leading empty entry", binDir, string(os.PathListSeparator) + filepath.Join(dir, "missing")},
		{"middle empty entry", binDir, filepath.Join(dir, "missing") + string(os.PathListSeparator) + string(os.PathListSeparator) + filepath.Join(dir, "missing-again")},
		{"trailing empty entry", binDir, filepath.Join(dir, "missing") + string(os.PathListSeparator)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RunWithEnv(context.Background(), tc.workDir, []string{"PATH=" + tc.path}, "version")
			if err != nil {
				t.Fatalf("git with the given environment: %v", err)
			}
			if out != "the-environment-git" {
				t.Fatalf("the command ran a git the given environment does not name: %q", out)
			}
		})
	}
	t.Run("bare", func(t *testing.T) {
		t.Chdir(binDir)
		out, err := RunWithEnv(context.Background(), ".", []string{"PATH=."}, "version")
		if err != nil {
			t.Fatalf("git with the current-directory PATH: %v", err)
		}
		if out != "the-environment-git" {
			t.Fatalf("the command ran a git the given environment does not name: %q", out)
		}
	})

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

func TestContextGitHelpersUseTheSuppliedEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix forwarding script")
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	repo := initTestRepo(t)
	head := run(t, repo, "git", "rev-parse", "HEAD")
	branch := run(t, repo, "git", "symbolic-ref", "HEAD")
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$PATH\" >> \"$ENV_GIT_LOG\"\nexec \"$ENV_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"run", "init bare", "existing ref", "missing ref", "transaction", "detached head"} {
		for _, path := range []string{bin, t.TempDir(), ""} {
			t.Run(operation+"/"+map[bool]string{true: "available", false: "missing"}[path == bin]+"/"+path, func(t *testing.T) {
				log := filepath.Join(t.TempDir(), "calls")
				ctx := WithEnvironment(context.Background(), runenv.Overlay{Set: map[string]string{
					"PATH": path, "ENV_REAL_GIT": realGit, "ENV_GIT_LOG": log,
				}})
				var err error
				switch operation {
				case "run":
					_, err = Run(ctx, repo, "version")
				case "init bare":
					bare := filepath.Join(t.TempDir(), "new.git")
					err = InitBare(ctx, bare)
					if err == nil {
						if out, checkErr := Run(context.Background(), bare, "rev-parse", "--is-bare-repository"); checkErr != nil || out != "true" {
							t.Fatalf("initialized repository is not bare: %q, %v", out, checkErr)
						}
					}
				case "existing ref", "missing ref":
					ref := "HEAD"
					if operation == "missing ref" {
						ref = "refs/heads/nonexistent"
					}
					var exists bool
					exists, err = RefExists(ctx, repo, ref)
					if err == nil && exists != (operation == "existing ref") {
						t.Fatalf("RefExists(%q) = %v", ref, exists)
					}
				case "transaction":
					called := false
					err = WithPreparedRefTransaction(ctx, repo, fmt.Sprintf("verify %s %s\n", branch, head), func() error { called = true; return nil })
					if called != (err == nil) {
						t.Fatalf("prepared callback ran=%v, transaction error=%v", called, err)
					}
				case "detached head":
					var detached bool
					detached, err = IsDetachedHEAD(ctx, repo)
					if err == nil && detached {
						t.Fatal("attached fixture reported detached")
					}
				}
				if path != bin {
					if err == nil {
						t.Fatal("helper used ambient Git when supplied PATH contained no Git")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				calls, err := os.ReadFile(log)
				if err != nil {
					t.Fatalf("supplied Git was not invoked: %v", err)
				}
				if string(calls) != path+"\n" {
					t.Fatalf("supplied Git child PATH = %q, want %q", calls, path+"\n")
				}
			})
		}
	}
}
