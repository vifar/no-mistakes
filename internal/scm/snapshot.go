package scm

import (
	"context"
	"sync"
)

type prViewSnapshotKey struct{}

type prViewSnapshot struct {
	mu      sync.Mutex
	entries map[string]any
}

// WithPRViewSnapshot returns a context in which a host may answer repeated
// reads of one PR's fields (state, base, mergeability, head) from a single
// forge view. The CI monitor opens one per poll: those reads describe one
// moment, and reading each field separately cost one API call apiece on every
// poll. A host without snapshot support ignores it, so nothing changes there.
// A snapshot must never span a write or a wait: it is the poll's own view.
func WithPRViewSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, prViewSnapshotKey{}, &prViewSnapshot{entries: map[string]any{}})
}

// HasPRViewSnapshot reports whether ctx carries a PR view snapshot.
func HasPRViewSnapshot(ctx context.Context) bool {
	_, ok := ctx.Value(prViewSnapshotKey{}).(*prViewSnapshot)
	return ok
}

type rememberedPRView[T any] struct {
	value T
	err   error
}

// RememberPRView returns the snapshot's answer for key, calling read on the
// first use. The answer, a failure included, is kept for the snapshot's
// lifetime, so one poll tries the combined view once; a host falls back to
// its per-field reads when it failed. Without a snapshot it always calls read.
func RememberPRView[T any](ctx context.Context, key string, read func() (T, error)) (T, error) {
	snapshot, ok := ctx.Value(prViewSnapshotKey{}).(*prViewSnapshot)
	if !ok {
		return read()
	}
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if remembered, ok := snapshot.entries[key].(rememberedPRView[T]); ok {
		return remembered.value, remembered.err
	}
	value, err := read()
	snapshot.entries[key] = rememberedPRView[T]{value: value, err: err}
	return value, err
}
