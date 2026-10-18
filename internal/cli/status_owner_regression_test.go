package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestHumanStatusAlwaysShowsLaunchOwner(t *testing.T) {
	for _, owner := range []string{"Firstmate", ""} {
		name := owner
		if name == "" {
			name = "ownerless"
		}
		t.Run(name, func(t *testing.T) {
			repoDir, _, database, repo := setupAxiQueryRepo(t)
			chdir(t, repoDir)
			writeGlobalConfig(t, "intent:\n  require_owner: false\n")
			if _, err := database.InsertRunWithIntent(repo.ID, "main", "head", "base", &db.RunIntent{Owner: owner}, ""); err != nil {
				t.Fatal(err)
			}
			out, err := executeCmd("status")
			if err != nil {
				t.Fatal(err)
			}
			want := owner
			if want == "" {
				want = "OWNERLESS"
			}
			if !strings.Contains(out, "owner:") || !strings.Contains(out, want) {
				t.Fatalf("missing explicit owner %q in status: %s", want, out)
			}
		})
	}
}
