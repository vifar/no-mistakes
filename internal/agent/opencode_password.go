package agent

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// opencodePasswordPattern matches the line an OpenCode server prints when
// it starts with a server password.
//
// Measured on 2.0.32: the server prints "server password <value>" on STDOUT
// when it is not in service mode, and it does so whether the password came
// from the environment or was generated at startup. The value is base64url
// encoded, so it carries no spaces.
var opencodePasswordPattern = regexp.MustCompile(`(?m)^server password (\S+)\s*$`)

// opencodePasswordCapture watches a managed server's output for the
// password it prints, and forwards every line to the real sink.
//
// The 2.x server generates a random password at startup unless one is
// configured, and it prints it once on stdout. A client that did not know
// it could only ever get a 401, which reads as a broken model rather than a
// credential it was never told. Capturing the printed value is therefore
// how the client and the server agree.
//
// The capture is a LINE reader, so the sink still sees the output in order
// and nothing is held back. A line that is not the password line is passed
// through untouched.
type opencodePasswordCapture struct {
	mu       sync.Mutex
	password string
	found    bool
	// pending holds the tail of a chunk that did not end on a line
	// boundary, so a password line split across two writes is still read
	// as one line.
	pending string
}

// newOpencodePasswordCapture returns a capture and the writer to hand to the
// server process.
func newOpencodePasswordCapture() (*opencodePasswordCapture, io.Writer) {
	capture := &opencodePasswordCapture{}
	return capture, capture
}

// Write forwards to the real sink and watches for the password line.
func (c *opencodePasswordCapture) Write(p []byte) (int, error) {
	c.scan(string(p))
	return c.sink().Write(p)
}

// sink returns the real output sink.
func (c *opencodePasswordCapture) sink() io.Writer {
	return currentManagedServerOutput()
}

// scan looks for the password in a chunk of output.
//
// The output arrives in arbitrary chunks, so a password line can be split
// across two of them. The scan therefore keeps the tail of the last chunk
// that did not end a line, and re-scans the joined text.
func (c *opencodePasswordCapture) scan(chunk string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.found {
		return
	}
	c.pending += chunk
	if !strings.HasSuffix(c.pending, "\n") {
		return
	}
	if match := opencodePasswordPattern.FindStringSubmatch(c.pending); match != nil {
		c.password = match[1]
		c.found = true
	}
	c.pending = ""
}

// Password returns the captured password, if the server printed one.
func (c *opencodePasswordCapture) Password() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.password
}

// Found reports whether the server printed a password.
func (c *opencodePasswordCapture) Found() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.found
}

// Wait blocks until the server printed its password, or until the deadline
// passes. It reports the password, which is empty when none arrived.
//
// The wait is bounded because the line may never come: a 1.x server prints
// no password at all, and a 2.x server that was given one in the
// environment does not print it either.
func (c *opencodePasswordCapture) Wait(ctx context.Context, d time.Duration) string {
	deadline := time.After(d)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if password := c.Password(); password != "" {
			return password
		}
		select {
		case <-ctx.Done():
			return c.Password()
		case <-deadline:
			return c.Password()
		case <-ticker.C:
		}
	}
}

// scanLines reads the output line by line and looks for the password. It is
// used for the case where the sink is a line-buffered writer and the
// password arrives on its own line.
func (c *opencodePasswordCapture) scanLines(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		c.scan(scanner.Text() + "\n")
	}
}

// opencodePasswordWait bounds how long the adapter waits for the server to
// print its password. The line is printed as the server comes up, so this
// only has to cover the last moment of startup.
const opencodePasswordWait = 5 * time.Second
