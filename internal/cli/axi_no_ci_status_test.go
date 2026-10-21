package cli

import (
	"testing"

	"github.com/toon-format/toon-go"
)

func TestOrdinaryStatusExposesCIReadinessReason(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ready, noCI bool
	}{
		{"waiting", false, false},
		{"declared no CI", true, true},
		{"real CI passed", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := axiDoc(runObjectField(runView{ID: "run", CIReady: tc.ready, CIReadyNoCI: tc.noCI}))
			var doc struct {
				Run struct {
					Ready *bool `toon:"ci_ready"`
					NoCI  *bool `toon:"ci_ready_no_ci"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(out, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Run.Ready == nil || doc.Run.NoCI == nil || *doc.Run.Ready != tc.ready || *doc.Run.NoCI != tc.noCI {
				t.Fatalf("status must distinguish waiting, real CI, and declared no CI:\n%s", out)
			}
		})
	}
}
