package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

type attributionPolicy struct {
	Submitted     string
	Configured    []string
	OriginalHooks string
	Targets       []string
}

type attributionSource struct {
	MessageFile string
	Original    string
}

var attributionObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func writeAttributionFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".attribution-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func HandleAttributionHook(args []string) (bool, error) {
	if len(args) == 0 || args[0] != "__sanitize-commit-message" {
		return false, nil
	}
	if len(args) < 3 {
		return true, fmt.Errorf("expected hook policy and hook name")
	}
	root := filepath.Dir(args[1])
	data, err := os.ReadFile(args[1])
	if err != nil {
		return true, err
	}
	var policy attributionPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return true, err
	}
	name, hookArgs := args[2], args[3:]
	run := func(args ...string) (string, error) { return Run(context.Background(), ".", args...) }
	var input []byte
	if name == "post-rewrite" {
		input, err = io.ReadAll(os.Stdin)
		if err != nil {
			return true, err
		}
	}
	source := attributionSource{}
	if name == "prepare-commit-msg" {
		if len(hookArgs) == 0 {
			return true, fmt.Errorf("missing commit message path")
		}
		source.MessageFile = hookArgs[0]
		if len(hookArgs) > 1 {
			original, readErr := run("rev-parse", "--verify", "REBASE_HEAD")
			if readErr == nil && attributionObjectID.MatchString(original) && policy.Submitted != "" {
				stoppedPath, pathErr := run("rev-parse", "--git-path", "rebase-merge/stopped-sha")
				stopped, stoppedErr := os.ReadFile(stoppedPath)
				supplied := false
				if hookArgs[1] == "commit" && len(hookArgs) > 2 {
					suppliedOriginal, err := run("rev-parse", "--verify", hookArgs[2]+"^{commit}")
					supplied = err == nil && suppliedOriginal == original
				}
				continuing := hookArgs[1] == "message" && strings.HasPrefix(os.Getenv("GIT_REFLOG_ACTION"), "rebase (")
				if pathErr == nil && stoppedErr == nil && strings.TrimSpace(string(stopped)) == original && (supplied || continuing) {
					if _, err := run("merge-base", "--is-ancestor", original, policy.Submitted); err == nil {
						source.Original = original
					}
				}
			}
		}
		encoded, _ := json.Marshal(source)
		if err := writeAttributionFile(filepath.Join(root, "source.json"), encoded, 0600); err != nil {
			return true, err
		}
	}
	cmdArgs := []string{"-c", "core.hooksPath=" + policy.OriginalHooks, "hook", "run", "--ignore-missing"}
	if name == "post-rewrite" {
		inputFile, err := os.CreateTemp(root, ".rewrite-input-")
		if err != nil {
			return true, err
		}
		defer os.Remove(inputFile.Name())
		if _, err := inputFile.Write(input); err != nil {
			inputFile.Close()
			return true, err
		}
		if err := inputFile.Close(); err != nil {
			return true, err
		}
		cmdArgs = append(cmdArgs, "--to-stdin="+inputFile.Name())
	}
	cmdArgs = append(cmdArgs, name, "--")
	cmdArgs = append(cmdArgs, hookArgs...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		return true, err
	}
	if name == "post-rewrite" && len(hookArgs) > 0 && hookArgs[0] == "rebase" {
		for _, line := range strings.Split(string(input), "\n") {
			pair := strings.Fields(line)
			if len(pair) != 2 || !attributionObjectID.MatchString(pair[0]) || !attributionObjectID.MatchString(pair[1]) {
				continue
			}
			if _, err := run("merge-base", "--is-ancestor", pair[0], policy.Submitted); err != nil {
				continue
			}
			if err := os.MkdirAll(filepath.Join(root, "replays"), 0700); err != nil {
				return true, err
			}
			if err := writeAttributionFile(filepath.Join(root, "replays", pair[1]), []byte(pair[0]), 0600); err != nil {
				return true, err
			}
		}
	}
	if name != "prepare-commit-msg" && name != "commit-msg" {
		return true, nil
	}
	if len(hookArgs) == 0 {
		return true, fmt.Errorf("missing commit message path")
	}
	if name == "commit-msg" {
		data, err := os.ReadFile(filepath.Join(root, "source.json"))
		if err != nil {
			return true, err
		}
		if err := json.Unmarshal(data, &source); err != nil {
			return true, err
		}
	}
	originalMessage := ""
	if source.Original != "" && source.MessageFile == hookArgs[0] {
		originalMessage, err = run("show", "-s", "--format=%B", source.Original)
		if err != nil {
			return true, err
		}
	}
	content, err := os.ReadFile(hookArgs[0])
	if err != nil {
		return true, err
	}
	cleaned := stripAttributionPreserving(string(content), originalMessage, policy.Configured)
	if cleaned == string(content) {
		return true, nil
	}
	return true, os.WriteFile(hookArgs[0], []byte(cleaned+"\n"), 0600)
}

func AttributionHookEnv(dir string, env []string, run func(...string) (string, error), submitted string, configured ...string) ([]string, error) {
	metadata, err := run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	root := filepath.Join(metadata, "no-mistakes-attribution")
	original, err := run("rev-parse", "--git-path", "hooks")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(original) {
		original = filepath.Join(dir, original)
	}
	original, err = filepath.Abs(original)
	if err != nil {
		return nil, err
	}
	if original == root {
		data, err := os.ReadFile(filepath.Join(root, "policy.json"))
		if err != nil {
			return nil, err
		}
		var old attributionPolicy
		if err := json.Unmarshal(data, &old); err != nil {
			return nil, err
		}
		original = old.OriginalHooks
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	targets, err := run("for-each-ref", "--format=%(objectname)", "refs/remotes/")
	if err != nil {
		return nil, err
	}
	policy := attributionPolicy{Submitted: submitted, Configured: configured, OriginalHooks: original, Targets: strings.Fields(targets)}
	data, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(original)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	names := map[string]bool{"prepare-commit-msg": true, "commit-msg": true, "post-rewrite": true}
	for _, entry := range entries {
		if !entry.IsDir() {
			names[entry.Name()] = true
		}
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	for name := range names {
		script := "#!/bin/sh\nexec " + quote(filepath.ToSlash(executable)) + " __sanitize-commit-message " + quote(filepath.ToSlash(filepath.Join(root, "policy.json"))) + " " + quote(name) + " \"$@\"\n"
		if err := writeAttributionFile(filepath.Join(root, name), []byte(script), 0700); err != nil {
			return nil, err
		}
	}
	if err := writeAttributionFile(filepath.Join(root, "policy.json"), data, 0600); err != nil {
		return nil, err
	}
	parameters := os.Getenv("GIT_CONFIG_PARAMETERS")
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "GIT_CONFIG_PARAMETERS="); ok {
			parameters = value
		}
	}
	env = append(append([]string(nil), env...), "GIT_CONFIG_PARAMETERS="+strings.TrimSpace(parameters+" "+quote("core.hooksPath="+filepath.ToSlash(root))))
	return env, nil
}

func ValidateCommitAttribution(before, after string, run func(...string) (string, error), submitted string, configured ...string) error {
	if before == "" || before == after {
		return nil
	}
	metadata, err := run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	root := filepath.Join(metadata, "no-mistakes-attribution")
	args := []string{"rev-list", "--first-parent", after, "^" + before}
	data, err := os.ReadFile(filepath.Join(root, "policy.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var policy attributionPolicy
		if err := json.Unmarshal(data, &policy); err != nil {
			return err
		}
		for _, target := range policy.Targets {
			if attributionObjectID.MatchString(target) {
				args = append(args, "^"+target)
			}
		}
	}
	commits, err := run(args...)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(commits) {
		message, err := run("show", "-s", "--format=%B", sha)
		if err != nil {
			return err
		}
		originalMessage := ""
		original, err := os.ReadFile(filepath.Join(root, "replays", sha))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && attributionObjectID.Match(original) && submitted != "" {
			if _, err := run("merge-base", "--is-ancestor", string(original), submitted); err != nil {
				return err
			}
			originalMessage, err = run("show", "-s", "--format=%B", string(original))
			if err != nil {
				return err
			}
		}
		if stripAttributionPreserving(message, originalMessage, configured) != message {
			return fmt.Errorf("refusing pipeline commit %s: AI attribution bypassed commit-time filtering", sha)
		}
	}
	return nil
}
