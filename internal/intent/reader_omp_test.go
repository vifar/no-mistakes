package intent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOmpReader_DiscoverAndLoadFixture(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".omp", "agent", "sessions", "-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-19T00-00-00.000Z_session-events.jsonl")
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	cwd, _ := json.Marshal(repoCWD)
	lines := []string{
		`{"type":"session","version":3,"id":"omp-session-1","timestamp":"` + timestamp + `","cwd":` + string(cwd) + `}`,
		`{"type":"message_end","id":"u1","timestamp":"` + timestamp + `","message":{"role":"user","id":"m1","content":[{"type":"text","text":"please add the intent reader in internal/intent/reader_omp.go"}]}}`,
		`{"type":"message_end","id":"u2","timestamp":"` + timestamp + `","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"I'll update the omp transcript reader."},{"type":"toolCall","name":"edit","arguments":{"file_path":"internal/intent/reader_omp.go"}}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := readerByName(t, AllReaders(nil), OmpReaderName)
	sessions, err := r.Discover(context.Background(), DiscoverOpts{
		HomeDir:     home,
		OriginCWD:   repoCWD,
		WindowStart: time.Now().Add(-time.Hour),
		WindowEnd:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].AgentName != OmpReaderName || sessions[0].SessionID != "omp-session-1" {
		t.Fatalf("unexpected session metadata: %+v", sessions[0])
	}
	if err := r.Load(context.Background(), sessions[0]); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sessions[0].Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(sessions[0].Messages), sessions[0].Messages)
	}
	if sessions[0].Messages[0].Role != RoleUser || !strings.Contains(sessions[0].Messages[0].Text, "reader_omp.go") {
		t.Errorf("user message = %+v", sessions[0].Messages[0])
	}
	if sessions[0].Messages[1].Role != RoleAssistant || !strings.Contains(sessions[0].Messages[1].Text, "transcript reader") {
		t.Errorf("assistant message = %+v", sessions[0].Messages[1])
	}
	if len(sessions[0].Messages[1].FilePaths) != 1 || sessions[0].Messages[1].FilePaths[0] != "internal/intent/reader_omp.go" {
		t.Errorf("tool paths = %v", sessions[0].Messages[1].FilePaths)
	}
}

func TestOmpReader_UnparseableSessionFailsClosed(t *testing.T) {
	repoCWD := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(home, ".omp", "agent", "sessions", "-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "broken.jsonl")
	cwd, _ := json.Marshal(repoCWD)
	content := `{"type":"session","version":3,"id":"broken","cwd":` + string(cwd) + `}` + "\n{not json}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewOmpReader()
	sessions, err := r.Discover(context.Background(), DiscoverOpts{HomeDir: home, OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if err := r.Load(context.Background(), sessions[0]); err == nil || !strings.Contains(err.Error(), "omp: session contains no parseable messages") {
		t.Fatalf("load error = %v, want clear parse failure", err)
	}
	empty, err := NewOmpReader().Discover(context.Background(), DiscoverOpts{HomeDir: t.TempDir(), OriginCWD: repoCWD})
	if err != nil {
		t.Fatalf("discover absent store: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("absent store returned %d sessions", len(empty))
	}
}
