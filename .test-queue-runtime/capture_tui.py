import fcntl, html, os, pathlib, pty, select, struct, subprocess, sys, termios, time
import pyte
binary, runid, outdir = sys.argv[1:]
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 35, 120, 0, 0))
def terminal_session():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)
p = subprocess.Popen([binary, 'attach', '--run', runid], stdin=slave, stdout=slave, stderr=slave, env=dict(os.environ, TERM='xterm-256color'), preexec_fn=terminal_session)
os.close(slave)
raw = bytearray()
screen = pyte.Screen(120, 35)
stream = pyte.ByteStream(screen)
try:
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if select.select([master], [], [], .1)[0]:
            try: data = os.read(master, 65536)
            except OSError: break
            raw.extend(data)
            if b"\x1b]11;?" in data: os.write(master,b"\x1b]11;rgb:1111/1515/1c1c\x1b\\")
            if b"\x1b[6n" in data: os.write(master,b"\x1b[1;1R")
            stream.feed(data)
        if p.poll() is not None: break
    lines = '\n'.join(screen.display)
    target = pathlib.Path(outdir)
    (target/'round2-queued-tui.ansi').write_bytes(raw)
    (target/'round2-queued-tui.txt').write_text(lines)
    colors = {'default':'#d8dee9','black':'#2e3440','red':'#bf616a','green':'#a3be8c','brown':'#ebcb8b','blue':'#81a1c1','magenta':'#b48ead','cyan':'#88c0d0','white':'#eceff4','brightblack':'#4c566a','brightred':'#bf616a','brightgreen':'#a3be8c','brightbrown':'#ebcb8b','brightblue':'#81a1c1','brightmagenta':'#b48ead','brightcyan':'#8fbcbb','brightwhite':'#eceff4'}
    def css_color(c):
        if c in colors: return colors[c]
        return '#'+c if len(c)==6 else colors['default']
    rows=[]
    for y in range(screen.lines):
        row=[]
        for x in range(screen.columns):
            c=screen.buffer[y][x]
            fg=css_color(c.fg)
            row.append('<span style="color:'+fg+(';font-weight:bold' if c.bold else '')+'">'+html.escape(c.data)+'</span>')
        rows.append(''.join(row))
    (target/'round2-queued-tui.html').write_text('<!doctype html><meta charset="utf-8"><title>Live queued-run terminal capture</title><style>body{background:#11151c;margin:24px}pre{font:16px/1.35 Menlo,monospace;color:#d8dee9;background:#11151c;white-space:pre}h1{color:#d8dee9;font:18px system-ui}</style><h1>Live no-mistakes TUI — queued run, 120 × 35 terminal</h1><pre>'+ '\n'.join(rows) + '</pre>')
    print(lines)
    if p.poll() is not None: raise SystemExit('TUI exited before capture')
    if 'Review' not in lines or 'Rebase' not in lines: raise SystemExit('queued pipeline placeholders missing')
finally:
    if p.poll() is None:
        os.write(master, b'q')
        deadline=time.monotonic()+3
        while p.poll() is None and time.monotonic()<deadline:
            if select.select([master],[],[],.1)[0]:
                try: os.read(master,65536)
                except OSError: break
        if p.poll() is None: p.terminate()
    p.wait(timeout=5)
    os.close(master)
