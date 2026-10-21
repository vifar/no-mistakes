package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func HandleAttributionHook(args []string) (bool, error) {
	if len(args) == 0 || args[0] != "__sanitize-commit-message" {
		return false, nil
	}
	if len(args) < 2 {
		return true, fmt.Errorf("expected a commit message file")
	}
	content, err := os.ReadFile(args[1])
	if err != nil {
		return true, err
	}
	cleaned := StripAIAttributionTrailers(string(content), args[2:]...)
	if cleaned == string(content) {
		return true, nil
	}
	return true, os.WriteFile(args[1], []byte(cleaned+"\n"), 0600)
}

func AttributionHookEnv(dir string, env []string, run func(...string) (string, error), configured ...string) ([]string, func(), error) {
	original, err := run("rev-parse", "--git-path", "hooks")
	if err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(original) {
		original = filepath.Join(dir, original)
	}
	original, err = filepath.Abs(original)
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	hooks, err := os.MkdirTemp("", "no-mistakes-attribution-hooks-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(hooks) }
	entries, err := os.ReadDir(original)
	if err != nil && !os.IsNotExist(err) {
		cleanup()
		return nil, nil, err
	}
	scripts := map[string]string{}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	for _, entry := range entries {
		info, statErr := os.Stat(filepath.Join(original, entry.Name()))
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			cleanup()
			return nil, nil, statErr
		}
		if info.IsDir() || info.Mode()&0111 == 0 {
			continue
		}
		scripts[entry.Name()] = quote(filepath.ToSlash(filepath.Join(original, entry.Name()))) + " \"$@\" || exit $?\n"
	}
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		scripts[name] += "exec " + quote(filepath.ToSlash(executable)) + " __sanitize-commit-message \"$1\""
		for _, source := range configured {
			scripts[name] += " " + quote(source)
		}
		scripts[name] += "\n"
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	parameters := os.Getenv("GIT_CONFIG_PARAMETERS")
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "GIT_CONFIG_PARAMETERS="); ok {
			parameters = value
		}
	}
	env = append(append([]string(nil), env...), "GIT_CONFIG_PARAMETERS="+strings.TrimSpace(parameters+" "+quote("core.hooksPath="+filepath.ToSlash(hooks))))
	return env, cleanup, nil
}

func ValidateCommitAttribution(before, after string, run func(...string) (string, error), configured ...string) error {
	if before == "" || before == after {
		return nil
	}
	commits, err := run("rev-list", "--first-parent", after, "^"+before)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(commits) {
		message, err := run("show", "-s", "--format=%B", sha)
		if err != nil {
			return err
		}
		if StripAIAttributionTrailers(message, configured...) != message {
			return fmt.Errorf("refusing pipeline commit %s: AI attribution bypassed commit-time filtering", sha)
		}
	}
	return nil
}
