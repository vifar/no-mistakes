package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// A post-receive notification describes refs Git has already changed. Owner
// validation must not discard it: admission must retain the validation failure.
func TestMalformedOwnerNotificationRetainsAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []string
	}{
		{"malformed_base64", []string{"no-mistakes.owner=%%%"}},
		{"duplicate", []string{formatOwnerPushOption("A"), formatOwnerPushOption("B")}},
		{"invalid_decoded", []string{formatOwnerPushOption("bad\nowner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var notification map[string]any
			p := legacyLifecycleServer(t, 0, func(s *ipc.Server) {
				s.Handle(ipc.MethodPushReceived, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					if err := json.Unmarshal(raw, &notification); err != nil {
						return nil, err
					}
					return &ipc.PushReceivedResult{RunID: "recorded-run"}, nil
				})
			})
			cmd := newDaemonNotifyPushCmd()
			args := []string{"--gate", p.RepoDir("fixture"), "--ref", "refs/heads/fresh", "--old", strings.Repeat("0", 40), "--new", strings.Repeat("1", 40)}
			for _, option := range tc.options {
				args = append(args, "--push-option", option)
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("changed ref stranded before notification: %v", err)
			}
			if notification["owner"] != "" || notification["owner_validation_error"] == nil || notification["owner_validation_error"] == "" {
				t.Fatalf("missing explicit ownerless/invalid-owner admission record: %#v", notification)
			}
		})
	}
}
