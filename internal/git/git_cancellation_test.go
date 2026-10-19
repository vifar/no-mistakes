package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestRunCancellationPreservesCommandProvenance(t *testing.T) {
	for _, call := range []struct {
		name string
		run  func(context.Context, string, ...string) error
	}{
		{"Run", func(ctx context.Context, dir string, args ...string) error {
			_, err := Run(ctx, dir, args...)
			return err
		}},
		{"RunRaw", func(ctx context.Context, dir string, args ...string) error {
			_, err := RunRaw(ctx, dir, args...)
			return err
		}},
		{"RunWithEnv", func(ctx context.Context, dir string, args ...string) error {
			_, err := RunWithEnv(ctx, dir, nil, args...)
			return err
		}},
	} {
		for _, mode := range []string{"ambiguous signal", "independent exit", "ambiguous cooperative exit", "independent signal", "cancelled context"} {
			t.Run(call.name+"/"+mode, func(t *testing.T) {
				realGit, err := testgit.RealGit()
				if err != nil {
					t.Fatal(err)
				}
				dir := initTestRepo(t)
				bin := t.TempDir()
				entered := filepath.Join(bin, "entered")
				script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\n", entered)
				if mode == "independent exit" {
					script = fmt.Sprintf("#!/bin/sh\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nexec %q \"$@\"\n", entered, realGit)
				} else if mode == "ambiguous cooperative exit" {
					script = fmt.Sprintf("#!/bin/sh\ntrap 'exit 17' TERM\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\n", entered)
				}
				if mode == "independent signal" {
					script = fmt.Sprintf("#!/bin/sh\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\n", entered)
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancelled context" {
					cancel()
				}
				done := make(chan error, 1)
				go func() {
					done <- call.run(ctx, dir, "ls-remote", "--exit-code", filepath.Join(t.TempDir(), "absent"), "refs/heads/main")
				}()
				if mode != "cancelled context" {
					deadline := time.After(10 * time.Second)
					for {
						raw, err := os.ReadFile(entered)
						if err == nil {
							pid, err := strconv.Atoi(string(raw))
							if err == nil && (mode != "independent exit" || errors.Is(commandProcessProbe(pid), syscall.ESRCH)) {
								break
							}
						}
						select {
						case err := <-done:
							t.Fatalf("command returned before race: %v", err)
						case <-deadline:
							t.Fatal("command did not reach barrier")
						case <-time.After(10 * time.Millisecond):
						}
					}
					if mode == "independent signal" {
						raw, err := os.ReadFile(entered)
						if err != nil {
							t.Fatal(err)
						}
						pid, err := strconv.Atoi(string(raw))
						if err != nil {
							t.Fatal(err)
						}
						process, err := os.FindProcess(pid)
						if err != nil {
							t.Fatal(err)
						}
						if err := process.Kill(); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
				}
				select {
				case err := <-done:
					var exit *exec.ExitError
					if mode != "cancelled context" && !errors.As(err, &exit) {
						t.Errorf("original command exit is not inspectable: %v", err)
					}
					if !errors.Is(err, context.Canceled) {
						t.Errorf("context cancellation metadata = %v: %v", errors.Is(err, context.Canceled), err)
					}
					if strings.Contains(err.Error(), "private-token") {
						t.Errorf("rendered error leaked credential: %v", err)
					}
					if mode == "independent exit" && (exit == nil || exit.ExitCode() != 128) {
						t.Errorf("independent exit lost: %v", err)
					}
					if mode == "independent signal" && exit != nil {
						status, ok := exit.Sys().(syscall.WaitStatus)
						if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
							t.Errorf("independent signal identity = %v", exit)
						}
					}
					if mode == "ambiguous cooperative exit" && (exit == nil || exit.ExitCode() != 17) {
						t.Errorf("ambiguous exit lost: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("command did not settle")
				}
			})
		}
	}
}

func commandProcessProbe(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) {
		return syscall.ESRCH
	}
	return err
}
