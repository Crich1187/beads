package tracker

import (
	"context"
	"strings"
)

// LastSyncKey returns the key under which a tracker's last successful sync
// timestamp is stored, e.g. "linear.last_sync".
func LastSyncKey(configPrefix string) string {
	return configPrefix + ".last_sync"
}

// ReadLastSync returns a tracker's last successful sync timestamp, or "" when
// no sync has been recorded.
//
// Engine.Sync writes the timestamp to local_metadata (clone-local and
// Dolt-ignored), so that is the authoritative location and is read first.
// Stores synced by builds that kept the timestamp in the config table still
// carry it there; config is the fallback so those stores keep reporting a
// last sync (and keep syncing incrementally) until the engine's next write
// lands in local_metadata. Every reader (the engine and each tracker's
// status command) must go through this helper: reading config alone is the
// bug that made `bd linear status` report an empty last_sync forever.
func ReadLastSync(ctx context.Context, store Store, configPrefix string) string {
	if store == nil {
		return ""
	}
	key := LastSyncKey(configPrefix)
	if value, err := store.GetLocalMetadata(ctx, key); err == nil {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if value, err := store.GetConfig(ctx, key); err == nil {
		return strings.TrimSpace(value)
	}
	return ""
}
