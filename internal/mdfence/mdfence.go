// Package mdfence tracks CommonMark fenced code blocks line by line.
//
// It is the single owner of fence detection for PR-body handling: the closing
// reference scanner uses it to skip fenced text, and every truncation site uses
// it to close a fence its cut left open. Sharing one implementation is what
// keeps the two from disagreeing about where a fence starts and ends, which is
// the disagreement that hid a requested "Closes #N" inside a fence the
// renderer itself had left open.
package mdfence

import "strings"

// Fence is the fenced-code-block state after the lines consumed so far. The
// zero value is outside any fence.
type Fence struct {
	marker byte
	width  int
}

// Consume advances the state past one line (without its newline). A line opens
// a fence with three or more backticks or tildes, indented by at most three
// spaces; a backtick fence's info string may not contain a backtick. It closes
// with a run of the same character at least as long as the opener and nothing
// after it but whitespace. Four-space-indented lines cannot open a fence.
func (f *Fence) Consume(raw string) {
	line := strings.TrimLeft(raw, " ")
	if len(raw)-len(line) > 3 || len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return
	}
	if f.marker == 0 {
		if line[0] == '`' && strings.ContainsRune(line[n:], '`') {
			return
		}
		f.marker, f.width = line[0], n
	} else if line[0] == f.marker && n >= f.width && strings.Trim(line[n:], " \t") == "" {
		f.marker, f.width = 0, 0
	}
}

// Open reports whether the state is inside a fence.
func (f Fence) Open() bool { return f.marker != 0 }

// Closer returns the line that closes the open fence, or "" outside a fence.
func (f Fence) Closer() string {
	if f.marker == 0 {
		return ""
	}
	return strings.Repeat(string(f.marker), f.width)
}

// Scan returns the state at the end of text.
func Scan(text string) Fence {
	var f Fence
	for _, line := range strings.Split(text, "\n") {
		f.Consume(line)
	}
	return f
}

// Close returns text unchanged when it ends outside a fence. Otherwise it
// returns text followed by a newline (when text does not already end in one)
// and the fence's closing line, so the result ends outside the fence.
func Close(text string) string {
	closer := Scan(text).Closer()
	if closer == "" {
		return text
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + closer
}
