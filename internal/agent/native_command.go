package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type nativeAgentCommand struct {
	cmd            *exec.Cmd
	stdout         *nativeAgentPipe
	stderr         *nativeAgentPipe
	waitCh         chan error
	terminateOnce  sync.Once
	closePipesOnce sync.Once
	pipeMu         sync.Mutex
	remainingPipes int
	pipesDone      chan struct{}
}

func writeNativeAgentStdin(stdin io.WriteCloser, prompt string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		closeErr := stdin.Close()
		errCh <- errors.Join(writeErr, closeErr)
	}()
	return errCh
}

type nativeAgentPipe struct {
	file         *os.File
	done         func()
	doneOnce     sync.Once
	drainMu      sync.Mutex
	drainTimeout time.Duration
	timedOut     atomic.Bool
	// activity, when set, is called for every non-empty read. It is the only
	// evidence no-mistakes has that a native agent is still alive during the
	// long tool-using stretches that produce no assistant prose.
	activity func()
}

func (p *nativeAgentPipe) Read(b []byte) (int, error) {
	// WaitDelay limits a blocked read after process exit, not the time the
	// parser spends in callbacks between reads. Refresh before each read so
	// already-written output survives slow logging or database callbacks.
	p.drainMu.Lock()
	if p.drainTimeout > 0 {
		_ = p.file.SetReadDeadline(time.Now().Add(p.drainTimeout))
	}
	p.drainMu.Unlock()
	n, err := p.file.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		p.timedOut.Store(true)
	}
	if n > 0 && p.activity != nil {
		p.activity()
	}
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeAgentPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeAgentPipe) markDone() {
	p.doneOnce.Do(p.done)
}

// startNativeAgentCommand starts cmd with dedicated stdout/stderr pipes.
// activity, when non-nil, is invoked on every non-empty read from either pipe;
// see LifecyclePhaseActivity for why subprocess byte liveness - not assistant
// prose - is the signal that distinguishes a working agent from a wedged one.
func startNativeAgentCommand(cmd *exec.Cmd, activity func()) (*nativeAgentCommand, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	oomBase, oomOK := shellenv.OOMKillBaseline()
	if err := shellenv.StartShellCommand(cmd); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeAgentCommand{
		cmd:            cmd,
		waitCh:         make(chan error, 1),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeAgentPipe{file: stdoutR, done: started.markPipeDone, activity: activity}
	started.stderr = &nativeAgentPipe{file: stderrR, done: started.markPipeDone, activity: activity}
	go func() {
		err := shellenv.AttributeOOMKill(oomBase, oomOK, cmd.Wait())
		started.terminate()
		started.waitCh <- started.waitForPipes(err)
	}()
	return started, nil
}

func (c *nativeAgentCommand) markPipeDone() {
	c.pipeMu.Lock()
	defer c.pipeMu.Unlock()
	c.remainingPipes--
	if c.remainingPipes == 0 {
		close(c.pipesDone)
	}
}

func (c *nativeAgentCommand) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// boundDrain bounds the idle time of each remaining Read after the leader
// exits and also interrupts a Read that was already blocked. It is an
// idle-read bound, not a total cap: a descendant that keeps writing is live
// output, and ctx cancellation (closePipes) still ends the invocation. Both
// pipes must be drained concurrently by the caller, as every adapter does;
// an undrained pipe never reports done. Unix os.Pipe readers support deadlines.
func (p *nativeAgentPipe) boundDrain(timeout time.Duration) {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()
	p.drainTimeout = timeout
	_ = p.file.SetReadDeadline(time.Now().Add(timeout))
}

func (c *nativeAgentCommand) waitForPipes(waitErr error) error {
	// Windows anonymous pipes do not support read deadlines. Retain their
	// existing wall-clock backstop rather than losing the inherited-handle bound.
	if runtime.GOOS == "windows" && c.cmd.WaitDelay > 0 {
		timer := time.NewTimer(c.cmd.WaitDelay)
		defer timer.Stop()
		select {
		case <-c.pipesDone:
			return waitErr
		case <-timer.C:
			c.closePipes()
			if waitErr == nil {
				return exec.ErrWaitDelay
			}
			return waitErr
		}
	}
	if c.cmd.WaitDelay > 0 {
		c.stdout.boundDrain(c.cmd.WaitDelay)
		c.stderr.boundDrain(c.cmd.WaitDelay)
	}
	<-c.pipesDone
	if waitErr == nil && (c.stdout.timedOut.Load() || c.stderr.timedOut.Load()) {
		return exec.ErrWaitDelay
	}
	return waitErr
}

func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		shellenv.TerminateShellCommandGroup(c.cmd)
	})
}

func (c *nativeAgentCommand) waitAfterParseError(parseErr error) error {
	c.terminate()
	c.closePipes()
	waitErr := c.wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return waitErr
	}
	return parseErr
}

func (c *nativeAgentCommand) wait() error {
	return <-c.waitCh
}

func (c *nativeAgentCommand) closePipes() {
	c.closePipesOnce.Do(func() {
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}
