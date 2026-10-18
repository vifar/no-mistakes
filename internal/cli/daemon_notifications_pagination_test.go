package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestNotificationListCLITransportsPagination(t *testing.T) {
	legacyLifecycleServer(t, 0, func(s *ipc.Server) {
		s.Handle(ipc.MethodListPushNotifications, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var p ipc.PushNotificationParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			if p.After != "opaque-cursor" || p.Limit != 7 {
				t.Errorf("page selection lost: %+v", p)
			}
			return []ipc.PushNotificationInfo{{ID: "next-id", NextCursor: "more.json", Pending: true}}, nil
		})
	})
	cmd := newDaemonNotificationsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "--after", "opaque-cursor", "--limit", "7"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var page []ipc.PushNotificationInfo
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].NextCursor != "more.json" {
		t.Fatalf("continuation lost: %s", out.String())
	}
}
