package main

import (
	"context"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
)

// Direct SQL-server config writes version themselves inside the store
// (DoltStore.SetConfig / DeleteConfig publish a post-transaction Dolt commit;
// config-LinearSync-001.30). The CLI's post-run auto-commit epilogue does not
// run on that route, so the only thing the command layer still owns is the
// auto-commit POLICY: in batch/off mode the write must stay in the working set
// for the explicit commit point, which the store learns from the context.

// directConfigWriteContext applies --dolt-auto-commit policy to a config write
// that goes through the store. The proxied route is left alone: its unit of
// work commits on its own terms and is documented as unaffected by the flag.
func directConfigWriteContext(ctx context.Context) (context.Context, error) {
	if usesProxiedServer() {
		return ctx, nil
	}
	return issueOpsContext(ctx)
}

// configWriteCommitter is implemented by stores whose config writes need an
// explicit commit point for a multi-key batch (the direct SQL-server store).
// Embedded stores do not implement it: their writes are committed by the
// post-run auto-commit epilogue, which includes config there.
type configWriteCommitter interface {
	CommitConfigWrites(ctx context.Context, keys []string, message string) error
}

// setConfigManyDirect writes a batch of database-backed config keys on the
// direct route as ONE Dolt commit (the property TestProxiedServerConfigSetMany
// pins for the proxied route): each key is written with its version commit
// deferred, then the batch is published once under the command's auto-commit
// policy.
func setConfigManyDirect(ctx context.Context, st storage.DoltStorage, keys, values []string) (failedKey string, err error) {
	writeCtx := storageissueops.WithDeferredVersionCommit(ctx)
	for i, key := range keys {
		if err := st.SetConfig(writeCtx, key, values[i]); err != nil {
			return key, err
		}
	}
	committer, ok := storage.UnwrapStore(st).(configWriteCommitter)
	if !ok {
		return "", nil
	}
	commitCtx, err := directConfigWriteContext(ctx)
	if err != nil {
		return "", err
	}
	return "", committer.CommitConfigWrites(commitCtx, keys, "bd: config set-many "+strings.Join(keys, ", "))
}
