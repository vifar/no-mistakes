package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func RegisterWorktreeSubmodules(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	raw, err := RunRaw(ctx, dir, "config", "--null", "--file", ".gitmodules", "--get-regexp", `^submodule\..*\.(url|update)$`)
	if err != nil && !configKeyMissing(err) {
		return err
	}
	for _, entry := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		key, value, ok := strings.Cut(entry, "\n")
		if !ok {
			continue
		}
		_, exists, err := submoduleConfigValue(ctx, dir, key)
		if err != nil {
			return err
		}
		if !exists {
			if strings.HasSuffix(key, ".url") {
				value, err = resolveSubmoduleURL(ctx, dir, value)
				if err != nil {
					return err
				}
			} else if strings.HasPrefix(value, "!") {
				value = "none"
			}
			if err := SetWorktreeConfig(ctx, dir, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func configKeyMissing(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

func submoduleConfigValue(ctx context.Context, dir, key string) (string, bool, error) {
	raw, err := RunRaw(ctx, dir, "config", "--null", "--get", key)
	if configKeyMissing(err) {
		return "", false, nil
	}
	return strings.TrimSuffix(string(raw), "\x00"), err == nil, err
}

func resolveSubmoduleURL(ctx context.Context, dir, value string) (string, error) {
	if !strings.HasPrefix(value, "./") && !strings.HasPrefix(value, "../") {
		return value, nil
	}
	remote := "origin"
	branch, err := Run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		if configured, exists, err := submoduleConfigValue(ctx, dir, "branch."+branch+".remote"); err != nil {
			return "", err
		} else if exists {
			remote = configured
		}
	} else if !configKeyMissing(err) {
		return "", err
	}
	base, exists, err := submoduleConfigValue(ctx, dir, "remote."+remote+".url")
	if err != nil {
		return "", err
	}
	if !exists {
		base, err = filepath.Abs(dir)
		if err != nil {
			return "", err
		}
	}
	base = strings.TrimSuffix(filepath.ToSlash(base), "/")
	relative := !filepath.IsAbs(base) && !strings.Contains(base, ":")
	if relative && !strings.HasPrefix(base, "./") && !strings.HasPrefix(base, "../") {
		base = "./" + base
	}
	separator := "/"
	for {
		if strings.HasPrefix(value, "./") {
			value = value[2:]
		} else if strings.HasPrefix(value, "../") {
			value = value[3:]
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[:i]
			} else if i := strings.LastIndex(base, ":"); i >= 0 {
				base = base[:i]
				separator = ":"
			} else if relative || base == "." {
				return "", fmt.Errorf("cannot resolve relative submodule URL")
			} else {
				base = "."
			}
		} else {
			break
		}
	}
	return strings.TrimPrefix(strings.TrimSuffix(base+separator+value, "/"), "./"), nil
}
