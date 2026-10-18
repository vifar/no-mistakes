package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestDaemonNotificationCommandsUseSupportedInterface(t *testing.T) {
	legacyLifecycleServer(t, 0, func(s *ipc.Server) {
		s.Handle(ipc.MethodListPushNotifications, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
			return []ipc.PushNotificationInfo{{ID: "retained-id", Pending: true}}, nil
		})
		s.Handle(ipc.MethodInspectPushNotification, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.PushNotificationParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			return &ipc.PushNotificationInfo{ID: params.ID, Pending: true, Params: &ipc.PushReceivedParams{OwnerValidationError: "duplicate owner option"}}, nil
		})
		s.Handle(ipc.MethodReplayPushNotification, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var params ipc.PushNotificationParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, err
			}
			if params.ID != "retained-id" {
				t.Errorf("replayed wrong identity: %s", params.ID)
			}
			return &ipc.PushReceivedResult{RunID: "admitted-run"}, nil
		})
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list"}, "retained-id"},
		{[]string{"inspect", "retained-id"}, "duplicate owner option"},
		{[]string{"replay", "retained-id"}, "admitted-run"},
	} {
		cmd := newDaemonNotificationsCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(tc.args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("unsupported recovery output: %s", out.String())
		}
	}
}
