package types

// RunEndingEvent is local audit evidence, not authorization. Caller is supplied
// by a client or explicitly labelled as a launch-owner fallback. An empty caller
// means no session or person is known; PeerPID identifies only the IPC process.
// The event describes the stop request, which may race an independent failure.
type RunEndingEvent struct {
	Event                           string  `json:"event" toon:"event"`
	Caller                          string  `json:"caller,omitempty" toon:"caller,omitempty"`
	Client                          string  `json:"client,omitempty" toon:"client,omitempty"`
	AttributionSource               string  `json:"attribution_source" toon:"attribution_source"`
	PeerPID                         int     `json:"peer_pid,omitempty" toon:"peer_pid,omitempty"`
	Reason                          string  `json:"reason" toon:"reason"`
	SupersedingRunID                string  `json:"superseding_run_id,omitempty" toon:"superseding_run_id,omitempty"`
	SupersedingSHA                  string  `json:"superseding_sha,omitempty" toon:"superseding_sha,omitempty"`
	SupersedingRef                  string  `json:"superseding_ref,omitempty" toon:"superseding_ref,omitempty"`
	SupersedingOldSHA               string  `json:"superseding_old_sha,omitempty" toon:"superseding_old_sha,omitempty"`
	SupersedingGeneration           *int64  `json:"superseding_generation,omitempty" toon:"superseding_generation,omitempty"`
	SupersedingValidationGeneration *string `json:"superseding_validation_generation,omitempty" toon:"superseding_validation_generation,omitempty"`
	PushNotificationID              string  `json:"push_notification_id,omitempty" toon:"push_notification_id,omitempty"`
	CreatedAt                       int64   `json:"created_at" toon:"created_at"`
}
