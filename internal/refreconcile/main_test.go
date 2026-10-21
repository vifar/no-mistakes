package refreconcile

import (
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	testgit.ClearConfigEnv()
	os.Exit(m.Run())
}
