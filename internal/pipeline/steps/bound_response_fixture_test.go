package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gateSnapshot(e interface{ CurrentGate() types.GateIdentity }) *types.GateIdentity {
	g := e.CurrentGate()
	return &g
}
