package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReplayFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadLabelsRefusesDuplicatePath(t *testing.T) {
	path := writeReplayFixture(t, "labels.jsonl", strings.Join([]string{
		`{"path":"a.go","relevant":true,"reason":"x"}`,
		`{"path":"a.go","relevant":true,"reason":"y"}`,
	}, "\n"))
	if _, err := readLabels(path); err == nil || !strings.Contains(err.Error(), "duplicate label for a.go") {
		t.Fatalf("want duplicate label refusal, got %v", err)
	}
}

func TestReplayRecordedRefusesMalformedSets(t *testing.T) {
	relevant := map[string]bool{"a.go": true, "b.go": false}
	record := func(candidates []string, listed []string) string {
		rec := recordedResponse{BaseSHA: "base", HeadSHA: "head", Listed: listed}
		for _, p := range candidates {
			rec.Candidates = append(rec.Candidates, struct {
				Path     string  `json:"path"`
				Coupling float64 `json:"coupling"`
				Excerpt  string  `json:"excerpt,omitempty"`
			}{Path: p})
		}
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return writeReplayFixture(t, "recorded.json", string(data))
	}
	cases := []struct {
		name       string
		candidates []string
		listed     []string
		wantErr    string
	}{
		{"label without candidate", []string{"a.go"}, nil, "label b.go is not a candidate"},
		{"duplicate candidate", []string{"a.go", "b.go", "a.go"}, nil, "duplicate candidate a.go"},
		{"listed not a candidate", []string{"a.go", "b.go"}, []string{"c.go"}, "listed c.go is not a candidate"},
		{"duplicate listed", []string{"a.go", "b.go"}, []string{"a.go", "a.go"}, "duplicate listed a.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, err := replayRecorded(record(tc.candidates, tc.listed), "base", "head", relevant)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
	order, count, _, _, _, scoreRanked, err := replayRecorded(record([]string{"a.go", "b.go"}, []string{"a.go"}), "base", "head", relevant)
	if err != nil || count != 2 || scoreRanked || len(order) != 1 || order[0] != "a.go" {
		t.Fatalf("well-formed record: order=%v count=%d scoreRanked=%v err=%v", order, count, scoreRanked, err)
	}
}
