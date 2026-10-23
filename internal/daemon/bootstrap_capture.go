package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

type bootstrapCapture struct {
	cmd     *exec.Cmd
	input   *os.File
	restore func() error
}

// bootstrapSinkReadinessTimeout bounds how long the daemon waits for its own
// log-sink helper to answer. It is a spawn budget on a shared machine, not a
// product deadline: the helper is this binary re-executed, and a loaded runner
// took longer than the original 5s to get it to print "ready".
const bootstrapSinkReadinessTimeout = 30 * time.Second

func startBootstrapCapture(p *paths.Paths) (*bootstrapCapture, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	sinkInput, input, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create bootstrap input: %w", err)
	}
	cmd := exec.Command(exe, "daemon", "log-sink", "--root", p.Root())
	cmd.Env = upsertEnv(os.Environ(), "NM_DAEMON_HELPER_PROCESS", "bootstrap-sink")
	cmd.Stdin = sinkInput
	cmd.Stderr = os.Stderr
	ready, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		_ = sinkInput.Close()
		return nil, fmt.Errorf("create bootstrap readiness pipe: %w", err)
	}
	winproc.Harden(cmd)
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		_ = sinkInput.Close()
		return nil, fmt.Errorf("start bootstrap sink: %w", err)
	}
	_ = sinkInput.Close()

	readyResult := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(ready).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected readiness response %q", line)
		}
		readyResult <- err
	}()
	// The sink is this same binary re-executed, so its readiness budget is a
	// process spawn plus path setup, not product work. 5s was measured too
	// small on the shared CI runner: `nm init` failed with "daemon child ...
	// exited before readiness" and the bootstrap log held "bootstrap sink
	// readiness timed out" while the race suite shared the host (CI run
	// 39963484774). A startup budget that a loaded machine cannot meet turns
	// load into a product failure, so it is sized well above the spawn.
	select {
	case err := <-readyResult:
		if err != nil {
			_ = input.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("bootstrap sink readiness: %w", err)
		}
	case <-time.After(bootstrapSinkReadinessTimeout):
		_ = input.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("bootstrap sink readiness timed out")
	}
	_ = ready.Close()

	restore, err := redirectProcessOutput(input)
	if err != nil {
		_ = input.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return &bootstrapCapture{cmd: cmd, input: input, restore: restore}, nil
}

func (c *bootstrapCapture) Close() error {
	if c == nil {
		return nil
	}
	restoreErr := c.restore()
	closeErr := c.input.Close()
	waitErr := c.cmd.Wait()
	return errors.Join(restoreErr, closeErr, waitErr)
}

func RunBootstrapLogSink() (retErr error) {
	protectBootstrapSink()
	p, err := paths.New()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}
	w, err := logstore.Open(p.DaemonBootstrapLog(), logstore.BootstrapPolicy())
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, w.Close()) }()
	if err := w.RotateNow(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		return err
	}
	_, err = io.Copy(w, os.Stdin)
	return err
}
