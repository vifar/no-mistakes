package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type pushRecoveryFixture struct {
	p           *paths.Paths
	d           *db.DB
	m           *RunManager
	params      *ipc.PushReceivedParams
	predecessor string
}

func newPushRecoveryFixture(t *testing.T, predecessor bool) *pushRecoveryFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("intent:\n  require_owner: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	gate := p.RepoDir("recovery")
	gitCmd(t, "", "init", "-b", "main", gate)
	gitCmd(t, gate, "config", "user.name", "Test")
	gitCmd(t, gate, "config", "user.email", "test@example.com")
	gitCmd(t, gate, "commit", "--allow-empty", "-m", "old")
	old := gitOutput(t, gate, "rev-parse", "HEAD")
	gitCmd(t, gate, "update-ref", "refs/heads/fresh", old)
	if _, err := d.InsertRepoWithID("recovery", gate, "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	h := newSchedHarness(1, false)
	h.tryAdmit("blocker")
	m.sched = h.slotScheduler
	f := &pushRecoveryFixture{p: p, d: d, m: m}
	if predecessor {
		f.predecessor, err = m.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/fresh", Old: strings.Repeat("0", 40), New: old, Owner: "Firstmate"})
		if err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, gate, "commit", "--allow-empty", "-m", "accepted receive")
	head := gitOutput(t, gate, "rev-parse", "HEAD")
	gitCmd(t, gate, "update-ref", "refs/heads/fresh", head, old)
	f.params = &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/fresh", Old: old, New: head, Intent: "review accepted submission"}
	return f
}

func (f *pushRecoveryFixture) assertPreserved(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("admission unexpectedly succeeded")
	}
	rows, readErr := f.d.GetRunsByRepo("recovery")
	count := 0
	if f.predecessor != "" {
		count = 1
	}
	if readErr != nil || len(rows) != count {
		t.Errorf("failed admission changed run rows: %+v %v", rows, readErr)
	}
	if f.predecessor != "" {
		run, _ := f.d.GetRun(f.predecessor)
		if run == nil || run.Status != types.RunQueued {
			t.Errorf("predecessor was superseded before admission: %+v", run)
		}
		if owned := f.m.runs[f.predecessor]; owned == nil || owned.state != ownershipQueued {
			t.Error("predecessor ownership lost")
		}
	}
	if head := gitOutput(t, f.params.Gate, "rev-parse", f.params.Ref); head != f.params.New {
		t.Error("accepted ref changed")
	}
	records, _ := filepath.Glob(filepath.Join(f.p.Root(), "push-notifications", "*.json"))
	if len(records) != count+1 {
		t.Errorf("accepted notification not retained independently: %v", records)
	}
	if !strings.Contains(err.Error(), "daemon notifications replay") {
		t.Errorf("missing supported resolution diagnostic: %v", err)
	}
}

func TestPushRecoveryAuditFailurePreservesPredecessor(t *testing.T) {
	for _, issue := range []string{"", "duplicate owner push option"} {
		t.Run(issue, func(t *testing.T) {
			f := newPushRecoveryFixture(t, true)
			f.params.OwnerValidationError = issue
			writer, err := sql.Open("sqlite", f.p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err = writer.Exec("CREATE TRIGGER reject_admission BEFORE INSERT ON owner_admission_events BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END"); err != nil {
				t.Fatal(err)
			}
			_, err = f.m.HandlePushReceived(context.Background(), f.params)
			f.assertPreserved(t, err)
		})
	}
}

func TestPushRecoveryConfigFailurePreservesPredecessor(t *testing.T) {
	for _, invalid := range []string{"malformed", "directory"} {
		for _, nonce := range []bool{false, true} {
			for _, issue := range []string{"", "invalid owner"} {
				t.Run(invalid+"/"+map[bool]string{true: "nonce", false: "ordinary"}[nonce]+"/"+issue, func(t *testing.T) {
					f := newPushRecoveryFixture(t, true)
					f.params.OwnerValidationError = issue
					if nonce {
						f.params.LaunchNonce = "recovery-nonce"
						f.params.ValidationGeneration = "recovery-generation"
					}
					if invalid == "directory" {
						if err := os.Remove(f.p.ConfigFile()); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(f.p.ConfigFile(), 0700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(f.p.ConfigFile(), []byte("intent: ["), 0600); err != nil {
						t.Fatal(err)
					}
					var err error
					func() {
						defer func() {
							if p := recover(); p != nil {
								t.Errorf("configuration failure panicked: %v", p)
							}
						}()
						_, err = f.m.HandlePushReceived(context.Background(), f.params)
					}()
					f.assertPreserved(t, err)
					if err != nil && !strings.Contains(err.Error(), "load global config") {
						t.Errorf("unnamed config error: %v", err)
					}
				})
			}
		}
	}
}
