#!/usr/bin/env python3
"""Drive the no-mistakes TUI through a real pty and press the yolo key.

argv: <no-mistakes binary> <workdir> <keys> <seconds-before-keys> <seconds-after-keys>

The pty is given a non-zero window size before the TUI reads its grid: a 0x0
grid makes bubbletea exit immediately, which would turn this live check into a
fake. The master side is drained continuously and the final screen is written
to stdout.

The pre-key and post-key waits are prompts, not clocks, when the caller names
one:

  NM_TUI_WAIT_BEFORE   text to wait for on screen before the keys are pressed
  NM_TUI_WAIT_AFTER    text to wait for on screen after the keys are pressed
  NM_TUI_WAIT_LIMIT    seconds either wait may take (default 90)

A fixed pre-wait is a bet on how fast the machine renders its first frame and
a fixed post-wait is a bet on how fast the daemon answers. Both bets lose on a
loaded runner: the key lands before the gate screen exists, the refusal never
appears, and the journey reports a product defect that was really a sleep that
expired (CI run 39963484774). With a prompt the wait ends as soon as the text
arrives and only expires when the TUI genuinely never got there, which is the
condition the test is asserting.
"""
import fcntl, os, pty, select, signal, struct, sys, termios, time

binary, workdir, keys, pre, post = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4]), float(sys.argv[5])

wait_before = os.environ.get("NM_TUI_WAIT_BEFORE", "")
wait_after = os.environ.get("NM_TUI_WAIT_AFTER", "")
wait_limit = float(os.environ.get("NM_TUI_WAIT_LIMIT", "90"))

pid, fd = pty.fork()
if pid == 0:
    os.chdir(workdir)
    os.environ["TERM"] = "xterm-256color"
    os.execv(binary, [binary, "attach"])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 160, 0, 0))

buf = bytearray()

def drain(seconds):
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.2)
        if fd in r:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                return
            if not chunk:
                return
            buf.extend(chunk)

def wait_for(marker, limit):
    """Drain until marker is on screen, or limit expires. Returns whether it was seen."""
    needle = marker.encode()
    end = time.time() + limit
    while True:
        if needle and needle in buf:
            return True
        remaining = end - time.time()
        if remaining <= 0:
            return bool(needle) and needle in buf
        drain(min(0.2, remaining))

def interrupted(signum, frame):
    raise SystemExit(128 + signum)

signal.signal(signal.SIGTERM, interrupted)
signal.signal(signal.SIGINT, interrupted)
try:
    if wait_before:
        wait_for(wait_before, max(pre, wait_limit))
    else:
        drain(pre)
    for key in keys:
        os.write(fd, key.encode())
        drain(0.4)
    if wait_after:
        wait_for(wait_after, wait_limit)
        drain(1.0)
    else:
        drain(post)
    os.write(fd, b"q")
    drain(1.0)
finally:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    os.waitpid(pid, 0)
    os.close(fd)
    sys.stdout.write(buf.decode("utf-8", "replace"))
