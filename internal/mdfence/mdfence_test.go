package mdfence

import (
	"strings"
	"testing"
)

func TestCloseTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"outside", "a\nb", "a\nb"},
		{"closed", "```\nx\n```", "```\nx\n```"},
		{"NBSP does not close", "```text\noutput\n```\u00a0", "```text\noutput\n```\u00a0\n```"},
		{"tab suffix closes", "```text\noutput\n```\t", "```text\noutput\n```\t"},
		{"spaces and tabs close tilde", "~~~\nx\n~~~~ \t ", "~~~\nx\n~~~~ \t "},
		{"Unicode space does not close tilde", "~~~\nx\n~~~\u2003", "~~~\nx\n~~~\u2003\n~~~"},
		{"four space closer stays open", "```\nx\n    ```", "```\nx\n    ```\n```"},
		{"open backtick", "```text\nx", "```text\nx\n```"},
		{"open tilde", "~~~\nx\n", "~~~\nx\n~~~"},
		{"longer fence", "````md\n```\ninner\n```\nmore", "````md\n```\ninner\n```\nmore\n````"},
		{"short run does not close", "````\n```", "````\n```\n````"},
		{"info string blocks close", "```\n```x\n", "```\n```x\n```"},
		{"backtick in backtick info is not a fence", "``` a`b\nx", "``` a`b\nx"},
		{"tilde does not close backtick", "```\n~~~\n", "```\n~~~\n```"},
		{"indented four spaces is not a fence", "    ```\nx", "    ```\nx"},
		{"indented three spaces opens", "   ```\nx", "   ```\nx\n```"},
	} {
		if got := Close(tc.in); got != tc.want {
			t.Errorf("%s: Close(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if Scan(Close(tc.in)).Open() {
			t.Errorf("%s: result still ends inside a fence", tc.name)
		}
	}
}

func TestCommonMarkFenceLineEndings(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, marker := range []string{"```", "~~~~"} {
			open := marker + "text" + ending + "output" + ending
			if got := Close(open); got != open+marker {
				t.Fatalf("Close(%q) = %q, want %q", open, got, open+marker)
			}
			for _, suffix := range []string{"", "\t", "\u00a0"} {
				body := strings.Join([]string{marker + "text", "output", marker + suffix, "postscript"}, ending)
				wantOpen := suffix == "\u00a0"
				if got := Scan(body).Open(); got != wantOpen {
					t.Fatalf("Scan(%q).Open() = %t, want %t", body, got, wantOpen)
				}
				want := body
				if wantOpen {
					want += "\n" + marker
				}
				if got := Close(body); got != want {
					t.Fatalf("Close(%q) = %q, want %q", body, got, want)
				}
			}
		}
	}
	var fence Fence
	fence.Consume("```text\r")
	fence.Consume("```\t\r")
	if fence.Open() {
		t.Fatal("a trailing CR line terminator must not invalidate a tab-suffixed closer")
	}
}
