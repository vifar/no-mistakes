package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestTargetAdvanceRewindsAppearInStatusFromBothSources(t *testing.T) {
	for _, view := range []runView{runViewFromIPC(&ipc.RunInfo{TargetAdvanceRewinds: 2}), runViewFromDB(&db.Run{TargetAdvanceRewinds: 2}, nil, nil)} {
		if out := axiDoc(runObjectField(view)); !strings.Contains(out, "target_advance_rewinds: 2") {
			t.Fatalf("status omitted rewind counter: %s", out)
		}
	}
}
