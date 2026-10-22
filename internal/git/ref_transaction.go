package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func WithPreparedRefTransaction(ctx context.Context, dir, commands string, prepared func() error) error {
	args := []string{"update-ref", "--stdin", "--no-deref"}
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	env := nonInteractiveEnvForContext(ctx, dir)
	cmd := gitCommand(ctx, dir, env, args...)
	cmd.Dir = dir
	cmd.Env = env
	winproc.Harden(cmd)
	shellenv.ConfigureShellCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return err
	}
	defer shellenv.TerminateShellCommandGroup(cmd)
	scanner := bufio.NewScanner(stdout)
	exchange := func(input, expected string) error {
		if _, err := io.WriteString(stdin, input); err != nil {
			return err
		}
		if !scanner.Scan() || scanner.Text() != expected {
			return fmt.Errorf("git ref transaction did not acknowledge %s", expected)
		}
		return nil
	}
	err = exchange("start\n", "start: ok")
	if err == nil {
		err = exchange(commands+"prepare\n", "prepare: ok")
	}
	if err == nil {
		err = prepared()
	}
	if err == nil {
		err = exchange("commit\n", "commit: ok")
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if err != nil {
		return fmt.Errorf("git ref transaction: %w: %s", err, stderr.String())
	}
	if waitErr != nil {
		return fmt.Errorf("git ref transaction: %w: %s", waitErr, stderr.String())
	}
	return nil
}
