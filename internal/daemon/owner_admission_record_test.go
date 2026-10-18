package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestOwnerAdmissionRecordSurvivesErrorLogging(t *testing.T) {
	for _, issue := range []string{"", "decode owner push option: invalid base64", "duplicate owner push option", "owner contains control characters"} {
		t.Run(issue, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), []byte("log_level: error\nintent:\n  require_owner: true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			gate := p.RepoDir("record")
			gitCmd(t, "", "init", "-b", "main", gate)
			gitCmd(t, gate, "config", "user.name", "Test")
			gitCmd(t, gate, "config", "user.email", "test@example.com")
			gitCmd(t, gate, "commit", "--allow-empty", "-m", "fixture")
			head := gitOutput(t, gate, "rev-parse", "HEAD")
			gitCmd(t, gate, "update-ref", "refs/heads/fresh", head)
			if _, err := d.InsertRepoWithID("record", gate, "https://example.com/repo.git", "main"); err != nil {
				t.Fatal(err)
			}
			m := NewRunManager(d, p, nil)
			h := newSchedHarness(1, false)
			h.tryAdmit("blocker")
			m.sched = h.slotScheduler
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
			defer slog.SetDefault(previous)
			raw, _ := json.Marshal(map[string]any{"gate": gate, "ref": "refs/heads/fresh", "old": strings.Repeat("0", 40), "new": head, "intent": "inspect", "owner_validation_error": issue})
			var params ipc.PushReceivedParams
			if err := json.Unmarshal(raw, &params); err != nil {
				t.Fatal(err)
			}
			id, err := m.HandlePushReceived(context.Background(), &params)
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.GetRun(id)
			if err != nil || run == nil || run.Owner() != "" {
				t.Fatalf("stranded or misowned run: %#v %v", run, err)
			}
			if strings.Contains(logs.String(), "launch_owner_policy_bypassed") {
				t.Fatal("fixture did not suppress warning logs")
			}
			uri := (&url.URL{Scheme: "file", Path: p.DB(), RawQuery: "mode=ro"}).String()
			reader, err := sql.Open("sqlite", uri)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var payload string
			if err := reader.QueryRow("SELECT payload FROM owner_admission_events WHERE run_id = ?", id).Scan(&payload); err != nil {
				t.Fatalf("no durable bypass event with log_level:error: %v", err)
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] != "launch_owner_policy_bypassed" || event["run_id"] != id || event["branch"] != "fresh" || event["ownerless_admitted"] != true || event["caller_path"] != "post-receive (TUI/direct push)" || event["owner_validation_error"] != issue {
				t.Fatalf("incomplete durable audit: %#v", event)
			}
		})
	}
}
