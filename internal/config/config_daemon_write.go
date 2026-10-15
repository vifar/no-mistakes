package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetGlobalDaemonSetting writes one daemon.<key> scalar into the global config
// at path, creating the file or the daemon block when absent. It edits the
// document as text, so comments, ordering and the operator's other settings are
// left byte-for-byte alone; only the one value line changes. The result is
// parsed with the same strict loader the daemon uses before anything is written,
// so a refused value or an unparseable result never reaches disk.
//
// A config file that is a symlink is updated through it: the target is replaced
// atomically, with its own mode, and the link stays a link.
func SetGlobalDaemonSetting(path, key, value string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve global config symlink: %w", err)
		}
		path = target
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read global config: %w", err)
	}
	// The config can hold secrets-adjacent settings, so a file the operator
	// made private stays private. Only a file created here gets the default.
	mode := fs.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	updated, err := setDaemonSettingText(string(data), key, value)
	if err != nil {
		return err
	}
	if _, err := LoadGlobalFromBytes([]byte(updated)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("write global config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		return fmt.Errorf("write global config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write global config: %w", err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("write global config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write global config: %w", err)
	}
	return nil
}

func setDaemonSettingText(text, key, value string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("global config is not valid YAML; fix it before changing daemon.%s: %w", key, err)
	}
	var mapping *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		mapping = doc.Content[0]
		if mapping.Kind != yaml.MappingNode {
			return "", errors.New("global config is not a mapping")
		}
	}
	appendBlock := func() string {
		out := text
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" {
			out += "\n"
		}
		return out + "daemon:\n  " + key + ": " + value + "\n"
	}
	if mapping == nil {
		return appendBlock(), nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "daemon" {
			continue
		}
		keyNode, block := mapping.Content[i], mapping.Content[i+1]
		lines := strings.Split(text, "\n")
		switch {
		case block.Kind == yaml.MappingNode && block.Style&yaml.FlowStyle == 0:
			for j := 0; j+1 < len(block.Content); j += 2 {
				if block.Content[j].Value != key {
					continue
				}
				v := block.Content[j+1]
				if v.Kind != yaml.ScalarNode || v.Style != 0 || v.Line != block.Content[j].Line {
					return "", fmt.Errorf("daemon.%s is written in a form this command will not rewrite; edit the global config by hand", key)
				}
				line := lines[v.Line-1]
				start := v.Column - 1
				lines[v.Line-1] = line[:start] + value + line[start+len(v.Value):]
				return strings.Join(lines, "\n"), nil
			}
			indent := 2
			if len(block.Content) > 0 && block.Content[0].Column > 1 {
				indent = block.Content[0].Column - 1
			}
			after := lastNodeLine(block)
			entry := strings.Repeat(" ", indent) + key + ": " + value
			lines = append(lines[:after], append([]string{entry}, lines[after:]...)...)
			return strings.Join(lines, "\n"), nil
		case block.Kind == yaml.ScalarNode && block.Tag == "!!null" && block.Line == keyNode.Line:
			// "daemon:" with nothing under it: its entry goes on the next line.
			entry := "  " + key + ": " + value
			lines = append(lines[:keyNode.Line], append([]string{entry}, lines[keyNode.Line:]...)...)
			return strings.Join(lines, "\n"), nil
		default:
			return "", errors.New("daemon is written in a form this command will not rewrite; edit the global config by hand")
		}
	}
	return appendBlock(), nil
}
