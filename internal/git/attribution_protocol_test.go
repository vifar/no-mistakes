package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAttributionDelegatedReferenceInput(t *testing.T) {
	dir := initTestRepo(t)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(hooks, "records")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + log + ".states'\ncat >> '" + log + ".'\"$1\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "config", "core.hooksPath", hooks)
	before := run(t, dir, "git", "rev-parse", "HEAD")
	run(t, dir, "git", "commit", "--allow-empty", "-m", "native oracle")
	after := run(t, dir, "git", "rev-parse", "HEAD")
	expected := map[string]string{}
	for _, state := range []string{"prepared", "committed"} {
		data, err := os.ReadFile(log + "." + state)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), before+" "+after+" ") {
			t.Fatalf("native Git oracle omitted ref update: %q", data)
		}
		expected[state] = strings.NewReplacer(before, "OLD", after, "NEW").Replace(string(data))
		if err := os.Remove(log + "." + state); err != nil {
			t.Fatal(err)
		}
	}
	nativeStates, err := os.ReadFile(log + ".states")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(log + ".states"); err != nil {
		t.Fatal(err)
	}
	env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.CommandContext(ctx, "git", "commit", "--allow-empty", "-m", "managed correction")
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, reader
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("managed commit with open caller stdin: %v %s", err, out)
	}
	next := run(t, dir, "git", "rev-parse", "HEAD")
	for _, state := range []string{"prepared", "committed"} {
		data, err := os.ReadFile(log + "." + state)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.NewReplacer(after, "OLD", next, "NEW").Replace(string(data))
		if got != expected[state] {
			t.Errorf("%s records = %q, native oracle = %q", state, got, expected[state])
		}
	}
	if data, err := os.ReadFile(log + ".states"); err != nil || string(data) != string(nativeStates) {
		t.Fatalf("transaction states = %q, %v", data, err)
	}
	for _, suffix := range []string{".prepared", ".committed", ".states"} {
		if err := os.Remove(log + suffix); err != nil {
			t.Fatal(err)
		}
	}
	foreign := initTestRepo(t)
	run(t, foreign, "git", "config", "core.hooksPath", hooks)
	foreignBefore := run(t, foreign, "git", "rev-parse", "HEAD")
	cmd = exec.Command("git", "commit", "--allow-empty", "-m", "foreign fixture")
	cmd.Dir, cmd.Env = foreign, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("foreign commit: %v %s", err, out)
	}
	foreignAfter := run(t, foreign, "git", "rev-parse", "HEAD")
	for _, state := range []string{"prepared", "committed"} {
		data, err := os.ReadFile(log + "." + state)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.NewReplacer(foreignBefore, "OLD", foreignAfter, "NEW").Replace(string(data))
		if got != expected[state] {
			t.Errorf("foreign %s records = %q, native = %q", state, got, expected[state])
		}
	}
	rejected := filepath.Join(hooks, "rejected")
	script = "#!/bin/sh\ncat > '" + rejected + "'\nexit 42\n"
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "update-ref", "refs/heads/rejected", next)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("reference veto lost: %s", out)
	}
	zero := strings.Repeat("0", len(next))
	if data, err := os.ReadFile(rejected); err != nil || string(data) != fmt.Sprintf("%s %s refs/heads/rejected\n", zero, next) {
		t.Fatalf("veto input = %q, %v", data, err)
	}
	if _, err := Run(context.Background(), dir, "rev-parse", "--verify", "refs/heads/rejected"); err == nil {
		t.Fatal("rejected ref created")
	}
	input := filepath.Join(t.TempDir(), "reference-input")
	if err := os.WriteFile(input, []byte("old new refs/heads/probe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "hook", "run", "--to-stdin="+input, "reference-transaction", "--", "prepared")
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("direct reference veto lost: %s", out)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 42 {
		t.Fatalf("direct reference hook status = %v", err)
	}
	if data, err := os.ReadFile(rejected); err != nil || string(data) != "old new refs/heads/probe\n" {
		t.Fatalf("direct reference input = %q, %v", data, err)
	}
}

func TestAttributionDelegatedCheckoutStatus(t *testing.T) {
	dir := initTestRepo(t)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	effect := filepath.Join(hooks, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + effect + "'\nprintf 'original stdout\\n'\nprintf 'original stderr\\n' >&2\nexit 37\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "config", "core.hooksPath", hooks)
	head := run(t, dir, "git", "rev-parse", "HEAD")
	native := exec.Command("git", "switch", "-c", "native-status")
	native.Dir = dir
	_, nativeErr := native.CombinedOutput()
	nativeExit, ok := nativeErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("native switch did not fail: %v", nativeErr)
	}
	nativeStatus := nativeExit.ExitCode()
	env, err := AttributionHookEnv(dir, os.Environ(), func(args ...string) (string, error) { return Run(context.Background(), dir, args...) }, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"checkout", "switch"} {
		cmd := exec.Command("git", "checkout", "--", "README.md")
		wantStatus, flag := 37, "0"
		if operation == "switch" {
			cmd = exec.Command("git", "switch", "-c", "status-switch")
			wantStatus, flag = nativeStatus, "1"
		}
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != wantStatus {
			t.Errorf("%s status = %v, want %d; output %s", operation, err, wantStatus, out)
		}
		if !strings.Contains(string(out), "original stdout\n") || !strings.Contains(string(out), "original stderr\n") {
			t.Errorf("original output lost: %s", out)
		}
		if data, err := os.ReadFile(effect); err != nil || string(data) != head+"\n"+head+"\n"+flag+"\n" {
			t.Fatalf("original arguments = %q %v", data, err)
		}
	}
	foreign := initTestRepo(t)
	run(t, foreign, "git", "config", "core.hooksPath", hooks)
	cmd := exec.Command("git", "switch", "-c", "foreign")
	cmd.Dir, cmd.Env = foreign, env
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("foreign hook failure swallowed")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != nativeStatus {
		t.Fatalf("foreign status = %v, native = %d", err, nativeStatus)
	}
}
