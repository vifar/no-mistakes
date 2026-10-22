package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	GitDir        string
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

type delegatedAttributionHookError struct {
	error
	code int
}

func AttributionHookResult(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if delegated, ok := err.(*delegatedAttributionHookError); ok {
		return delegated.code
	}
	fmt.Fprintln(stderr, err)
	return 1
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
	metadata, err := run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return true, err
	}
	metadata, err = filepath.EvalSymlinks(metadata)
	if err != nil {
		return true, err
	}
	if policy.GitDir == "" || metadata != policy.GitDir {
		return true, fmt.Errorf("attribution hook does not own this Git directory")
	}
	var input []byte
	stdinInfo, err := os.Stdin.Stat()
	if err != nil {
		return true, err
	}
	if stdinInfo.Mode()&os.ModeCharDevice == 0 {
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
		if len(hookArgs) > 1 && (hookArgs[1] == "message" || hookArgs[1] == "commit" || hookArgs[1] == "merge") {
			donePath, err := run("rev-parse", "--git-path", "rebase-merge/done")
			if err != nil {
				return true, err
			}
			done, readErr := os.ReadFile(donePath)
			if readErr != nil && !os.IsNotExist(readErr) {
				return true, readErr
			}
			lines := strings.Split(strings.TrimSpace(string(done)), "\n")
			command := strings.Fields(lines[len(lines)-1])
			candidate := ""
			if len(command) >= 2 && (command[0] == "pick" || command[0] == "p") && hookArgs[1] != "merge" {
				candidate = command[1]
			} else if len(command) >= 3 && (command[0] == "merge" || command[0] == "m") && (command[1] == "-C" || command[1] == "-c") {
				candidate = command[2]
			}
			if readErr == nil && candidate != "" {
				original, err := run("rev-parse", "--verify", candidate+"^{commit}")
				if err != nil {
					return true, err
				}
				stoppedPath, err := run("rev-parse", "--git-path", "rebase-merge/stopped-sha")
				if err != nil {
					return true, err
				}
				stopped, stoppedErr := os.ReadFile(stoppedPath)
				if stoppedErr != nil && !os.IsNotExist(stoppedErr) {
					return true, stoppedErr
				}
				stoppedOriginal := strings.TrimSpace(string(stopped))
				replaying := os.IsNotExist(stoppedErr) || stoppedOriginal != original || os.Getenv("GIT_REFLOG_ACTION") == "rebase (continue)"
				if hookArgs[1] == "commit" {
					if len(hookArgs) < 3 {
						replaying = false
					} else {
						supplied, err := run("rev-parse", "--verify", hookArgs[2]+"^{commit}")
						if err != nil {
							return true, err
						}
						replaying = replaying && supplied == original
					}
				}
				if replaying {
					origHeadPath, err := run("rev-parse", "--git-path", "rebase-merge/orig-head")
					if err != nil {
						return true, err
					}
					origHead, err := os.ReadFile(origHeadPath)
					if err != nil {
						return true, err
					}
					if !attributionObjectID.Match(bytes.TrimSpace(origHead)) {
						return true, fmt.Errorf("invalid rebase original head")
					}
					if _, err := run("merge-base", "--is-ancestor", original, strings.TrimSpace(string(origHead))); err != nil {
						return true, err
					}
					source.Original, err = existingReplayOriginal(root, original, policy, run)
					if err != nil {
						return true, err
					}
				}
			}
		}
		encoded, _ := json.Marshal(source)
		if err := writeAttributionFile(filepath.Join(root, "source.json"), encoded, 0600); err != nil {
			return true, err
		}
	}
	if name == "prepare-commit-msg" && source.Original == "" {
		content, err := os.ReadFile(hookArgs[0])
		if err != nil {
			return true, err
		}
		cleaned := StripAIAttributionTrailers(string(content), policy.Configured...)
		if cleaned != string(content) {
			if err := os.WriteFile(hookArgs[0], []byte(cleaned+"\n"), 0600); err != nil {
				return true, err
			}
		}
	}
	cmdArgs := []string{"-c", "core.hooksPath=" + policy.OriginalHooks, "hook", "run", "--ignore-missing"}
	inputFile, err := os.CreateTemp(root, ".hook-input-")
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
	cmdArgs = append(cmdArgs, name, "--")
	cmdArgs = append(cmdArgs, hookArgs...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	winproc.Harden(cmd)
	hookErr := cmd.Run()
	if exitErr, ok := hookErr.(*exec.ExitError); ok && exitErr.ExitCode() > 0 {
		hookErr = &delegatedAttributionHookError{error: hookErr, code: exitErr.ExitCode()}
	}
	if hookErr != nil && name != "post-rewrite" {
		return true, hookErr
	}
	if name == "post-rewrite" && len(hookArgs) > 0 && hookArgs[0] == "rebase" {
		for _, line := range strings.Split(string(input), "\n") {
			pair := strings.Fields(line)
			if len(pair) != 2 || !attributionObjectID.MatchString(pair[0]) || !attributionObjectID.MatchString(pair[1]) {
				continue
			}
			original, err := existingReplayOriginal(root, pair[0], policy, run)
			if err != nil {
				return true, errors.Join(hookErr, err)
			}
			if original == "" {
				continue
			}
			prior, err := existingReplayOriginal(root, pair[1], policy, run)
			if err != nil {
				return true, errors.Join(hookErr, err)
			}
			if prior != "" {
				canonical, err := attributionCommitMessage(prior, run)
				if err != nil {
					return true, errors.Join(hookErr, err)
				}
				message, err := attributionCommitMessage(pair[1], run)
				if err != nil {
					return true, errors.Join(hookErr, err)
				}
				if bytes.Equal(message, canonical) {
					continue
				}
			}
			if err := os.MkdirAll(filepath.Join(root, "replays"), 0700); err != nil {
				return true, errors.Join(hookErr, err)
			}
			if err := writeAttributionFile(filepath.Join(root, "replays", pair[1]), []byte(original), 0600); err != nil {
				return true, errors.Join(hookErr, err)
			}
		}
	}
	if name != "prepare-commit-msg" && name != "commit-msg" {
		return true, hookErr
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
	if source.Original != "" && source.MessageFile == hookArgs[0] {
		message, err := attributionCommitMessage(source.Original, run)
		if err != nil {
			return true, err
		}
		return true, os.WriteFile(hookArgs[0], message, 0600)
	}

	content, err := os.ReadFile(hookArgs[0])
	if err != nil {
		return true, err
	}
	cleaned := StripAIAttributionTrailers(string(content), policy.Configured...)
	if cleaned == string(content) {
		return true, nil
	}
	return true, os.WriteFile(hookArgs[0], []byte(cleaned+"\n"), 0600)
}

func AttributionHookEnv(dir string, env []string, run func(...string) (string, error), submitted string, imported []string, configured ...string) ([]string, error) {
	metadata, err := run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	metadata, err = filepath.EvalSymlinks(metadata)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(metadata, "no-mistakes-attribution")
	var previous attributionPolicy
	previousData, readErr := os.ReadFile(filepath.Join(root, "policy.json"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if readErr == nil {
		if err := json.Unmarshal(previousData, &previous); err != nil {
			return nil, err
		}
	}
	if submitted == "" {
		submitted = previous.Submitted
		if submitted == "" {
			submitted, err = run("rev-parse", "HEAD")
			if err != nil {
				return nil, err
			}
		}
	}
	if previous.Submitted != "" && previous.Submitted != submitted {
		return nil, fmt.Errorf("attribution boundary belongs to a different submitted head")
	}
	original, err := RunWithEnv(context.Background(), dir, env, "rev-parse", "--git-path", "hooks")
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
		if previous.OriginalHooks == "" {
			return nil, fmt.Errorf("missing original hook boundary")
		}
		original = previous.OriginalHooks
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	protected := append(append([]string(nil), previous.Targets...), imported...)
	seen := map[string]bool{}
	pinned := make([]string, 0, len(protected))
	for _, target := range protected {
		if !seen[target] {
			seen[target] = true
			pinned = append(pinned, target)
		}
	}
	policy := attributionPolicy{GitDir: metadata, Submitted: submitted, Configured: configured, OriginalHooks: original, Targets: pinned}
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
	configPath := filepath.Join(root, "config")
	content := fmt.Sprintf("[core]\n\thooksPath = %q\n[commit]\n\tcleanup = verbatim\n", filepath.ToSlash(root))
	if err := writeAttributionFile(configPath, []byte(content), 0600); err != nil {
		return nil, err
	}
	pattern := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[").Replace(filepath.ToSlash(metadata))
	env = append(append([]string(nil), env...), "GIT_CONFIG_PARAMETERS="+strings.TrimSpace(parameters+" "+quote("includeIf.gitdir:"+pattern+".path")+"="+quote(filepath.ToSlash(configPath))))
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
	metadata, err = filepath.EvalSymlinks(metadata)
	if err != nil {
		return err
	}
	root := filepath.Join(metadata, "no-mistakes-attribution")
	args := []string{"rev-list", after, "^" + before}
	data, err := os.ReadFile(filepath.Join(root, "policy.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	policy := attributionPolicy{Submitted: submitted}
	if err == nil {
		if err := json.Unmarshal(data, &policy); err != nil {
			return err
		}
		if submitted != "" && policy.Submitted != "" && submitted != policy.Submitted {
			return fmt.Errorf("attribution validation belongs to a different submitted head")
		}
		if policy.Submitted == "" {
			policy.Submitted = submitted
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
		message, err := attributionCommitMessage(sha, run)
		if err != nil {
			return err
		}
		original, err := existingReplayOriginal(root, sha, policy, run)
		if err != nil {
			return err
		}
		if original != "" {
			canonical, err := attributionCommitMessage(original, run)
			if err != nil {
				return err
			}
			if !bytes.Equal(message, canonical) {
				return fmt.Errorf("refusing replay %s: original commit message changed", sha)
			}
		} else if StripAIAttributionTrailers(string(message), configured...) != string(message) {
			return fmt.Errorf("refusing pipeline commit %s: AI attribution bypassed commit-time filtering", sha)
		}
	}
	return nil
}

func attributionCommitMessage(sha string, run func(...string) (string, error)) ([]byte, error) {
	framed, err := run("show", "-s", "--encoding=none", "--format=format:%x00%B%x00", sha)
	if err != nil {
		return nil, err
	}
	if len(framed) < 2 || framed[0] != 0 || framed[len(framed)-1] != 0 {
		return nil, fmt.Errorf("invalid raw commit message framing")
	}
	return []byte(framed[1 : len(framed)-1]), nil
}

func existingReplayOriginal(root, candidate string, policy attributionPolicy, run func(...string) (string, error)) (string, error) {
	if !attributionObjectID.MatchString(candidate) {
		return "", nil
	}
	contained := func(sha string) (bool, error) {
		roots := append([]string{policy.Submitted}, policy.Targets...)
		for _, tip := range roots {
			if !attributionObjectID.MatchString(tip) {
				continue
			}
			_, err := run("merge-base", "--is-ancestor", sha, tip)
			if err == nil {
				return true, nil
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				return false, err
			}
		}
		return false, nil
	}
	known, err := contained(candidate)
	if err != nil {
		return "", err
	}
	if known {
		return candidate, nil
	}
	original, err := os.ReadFile(filepath.Join(root, "replays", candidate))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	canonical := strings.TrimSpace(string(original))
	if !attributionObjectID.MatchString(canonical) {
		return "", fmt.Errorf("invalid existing replay provenance")
	}
	known, err = contained(canonical)
	if err != nil {
		return "", err
	}
	if !known {
		return "", fmt.Errorf("replay provenance is outside preserved history")
	}
	return canonical, nil
}
