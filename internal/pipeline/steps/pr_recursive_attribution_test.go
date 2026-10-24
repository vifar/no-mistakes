package steps

import (
	"html"
	"testing"
)

func TestPRRecursiveAttributionViewIsIdempotent(t *testing.T) {
	for _, input := range []string{
		"<code>co-author (quoted): <code>Codex</code> <ai@example.com></code>",
		"**co-author (quoted): **Codex** <ai@example.com>**",
		"``<code>**co-author (quoted): `<code>Codex</code>` <code>\"<ai@example.com>\"</code>**</code>``",
		"co-author (quoted): **Codex** **<ai@example.com>**",
		"co-author (quoted): <code>Codex</code> <code><ai@example.com></code>",
		"co-author (quoted): <code>Codex <code><ai@example.com></code></code>",
	} {
		for _, raw := range []string{input, html.EscapeString(input)} {
			view, _ := reviewAttributionView(raw)
			if view != "Co-Authored-By: Codex <ai@example.com>" {
				t.Errorf("classification view = %q for %q", view, raw)
			}
			again, _ := reviewAttributionView(view)
			if again != view {
				t.Errorf("normalization changed its own view: %q -> %q", view, again)
			}
			if got := prDisplayText(raw); got != "" {
				t.Errorf("nested AI attribution survived: %q", got)
			}
		}
	}
}
