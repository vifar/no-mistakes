package mdfence

import "testing"

func TestCloseTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"outside", "a\nb", "a\nb"},
		{"closed", "```\nx\n```", "```\nx\n```"},
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
