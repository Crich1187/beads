package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/linear"
	"github.com/steveyegge/beads/internal/tracker"
)

// lastSyncMetaStore holds local_metadata only; the helpers under test touch
// nothing else.
type lastSyncMetaStore struct {
	tracker.Store
	meta map[string]string
}

func (s *lastSyncMetaStore) GetLocalMetadata(_ context.Context, key string) (string, error) {
	return s.meta[key], nil
}

func (s *lastSyncMetaStore) SetLocalMetadata(_ context.Context, key, value string) error {
	s.meta[key] = value
	return nil
}

// bd linear sync defers last_sync out of engine.Sync (acceptance run 2,
// finding N1); finishLinearSyncLastSync records it after the passes.
func TestLinearSyncEngineDefersLastSync(t *testing.T) {
	engine := newLinearSyncEngine(&linear.Tracker{}, &lastSyncMetaStore{meta: map[string]string{}}, "test")
	if !engine.DeferLastSync || !engine.ThreeWayLabelMerge {
		t.Fatalf("bd linear sync engine: DeferLastSync=%v ThreeWayLabelMerge=%v, want both true", engine.DeferLastSync, engine.ThreeWayLabelMerge)
	}
}

// last_sync is later than every write the post-sync passes make, including
// when a pass fails, and dry runs record nothing.
func TestLinearSyncRecordsLastSyncAfterPostSyncPasses(t *testing.T) {
	ctx := context.Background()
	newEngine := func() (*tracker.Engine, *lastSyncMetaStore) {
		st := &lastSyncMetaStore{meta: map[string]string{}}
		return newLinearSyncEngine(&linear.Tracker{}, st, "test"), st
	}
	// A pass whose write lands past the second a stamp taken before it would
	// carry, so only the ordering decides whether last_sync covers it.
	var passWrite time.Time
	pass := func(err error) func() error {
		return func() error {
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(1050 * time.Millisecond)))
			passWrite = time.Now().UTC()
			return err
		}
	}

	for _, tc := range []struct {
		name    string
		passErr error
	}{{"passes_ok", nil}, {"pass_fails", errors.New("relation reconcile failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			engine, st := newEngine()
			result := &tracker.SyncResult{Success: true}
			err := finishLinearSyncLastSync(ctx, engine, result, false, pass(tc.passErr))
			if !errors.Is(err, tc.passErr) {
				t.Fatalf("error = %v, want the pass error %v", err, tc.passErr)
			}
			stored := st.meta["linear.last_sync"]
			lastSync, perr := time.Parse(time.RFC3339Nano, stored)
			if perr != nil {
				t.Fatalf("last_sync %q not recorded: %v", stored, perr)
			}
			if !lastSync.After(passWrite) {
				t.Fatalf("last_sync %s is not after the pass write %s", lastSync, passWrite)
			}
			if result.LastSync != stored {
				t.Fatalf("result.LastSync = %q, want the recorded %q", result.LastSync, stored)
			}
		})
	}

	t.Run("dry_run", func(t *testing.T) {
		engine, st := newEngine()
		if err := finishLinearSyncLastSync(ctx, engine, &tracker.SyncResult{Success: true}, true, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if v, ok := st.meta["linear.last_sync"]; ok {
			t.Fatalf("dry run recorded last_sync %q", v)
		}
	})
}
