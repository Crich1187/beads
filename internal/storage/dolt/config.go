package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// SetConfig sets a configuration value and publishes it as a Dolt version
// commit (config-LinearSync-001.30, recurrence of root-c1q3p).
//
// Durability contract: on a nil return the row is in HEAD, not merely in the
// branch working set -- unless ctx defers version commits
// (issueops.WithDeferredVersionCommit: --dolt-auto-commit batch/off), in which
// case the write is left in the working set for the caller's explicit commit
// point, exactly like the issue-operation write paths.
//
// Why the store must do this itself: DoltStore is the direct SQL-server
// route, and on that route the CLI's post-run auto-commit epilogue
// (cmd/bd maybeAutoCommit) deliberately does nothing -- it relies on the
// storage layer versioning its own writes in server mode. Every other
// DoltStore write verb honours that (runIssueOperationTxWithMessage ->
// doltAddAndCommitPostTx), but SetConfig/DeleteConfig only committed the SQL
// transaction, so `bd config set`, `bd kv set` and tracker last_sync writes
// stranded config (and the custom_statuses / custom_types projections) in the
// shared working set until some unrelated sweep committed them.
//
// Ordering follows upstream #6040: the SQL transaction commits first, then the
// Dolt commit stages ONLY the tables this write touched (config plus its
// projection table, never a concurrent writer's dirty tables -- GH#2455) from
// the post-merge working set.
//
// Unlike the issue-operation path, a failed version commit is returned, not
// swallowed: a config write is idempotent (re-running it cannot double-apply),
// and reporting success for a value that is not in HEAD is the exact failure
// this contract exists to prevent.
func (s *DoltStore) SetConfig(ctx context.Context, key, value string) error {
	var projection string
	if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		if err := issueops.SetConfigInTx(ctx, tx, key, value); err != nil {
			return err
		}
		// Sync normalized tables when config keys change
		var err error
		projection, err = issueops.SyncConfigTables(ctx, tx, key, value)
		return err
	}); err != nil {
		return err
	}

	// Invalidate caches for keys that affect cached data
	s.invalidateConfigCaches(key)

	return s.publishConfigWrite(ctx, configWriteTables(projection), "bd: config set "+key)
}

// CommitConfigWrites publishes config writes that were made under a deferred
// version-commit context as ONE Dolt commit, staging only config and the
// projection tables the given keys feed. It is the commit point for a
// multi-key write (bd config set-many on the direct SQL-server route), so the
// batch stays one commit instead of one per key. It honours
// issueops.WithDeferredVersionCommit on its own ctx, and nothing staged means
// no commit.
func (s *DoltStore) CommitConfigWrites(ctx context.Context, keys []string, message string) error {
	var projections []string
	for _, key := range keys {
		if t := issueops.ConfigProjectionTable(key); t != "" {
			projections = append(projections, t)
		}
	}
	return s.publishConfigWrite(ctx, configWriteTables(projections...), message)
}

// configWriteTables is the staged set for a config write: config plus any
// non-empty, de-duplicated projection tables.
func configWriteTables(projections ...string) []string {
	tables := []string{"config"}
	for _, t := range projections {
		if t != "" && !slices.Contains(tables, t) {
			tables = append(tables, t)
		}
	}
	return tables
}

// publishConfigWrite creates the post-transaction Dolt commit for a config
// write. See SetConfig for the contract.
func (s *DoltStore) publishConfigWrite(ctx context.Context, tables []string, message string) error {
	if issueops.VersionCommitDeferred(ctx) {
		return nil
	}
	if err := s.doltAddAndCommitPostTx(ctx, tables, message); err != nil {
		return fmt.Errorf("config written to the working set but its Dolt commit failed "+
			"(readable now, missing from HEAD and from clones until committed; retry the write or run `bd dolt commit`): %w", err)
	}
	return nil
}

// invalidateConfigCaches drops the store-level caches derived from a config
// key. Every path that writes config — store-level SetConfig and
// doltTransaction.SetConfig alike — must call this, or a long-lived process
// (server/daemon) keeps serving the pre-write set from GetCustomTypes /
// GetCustomStatuses / GetInfraTypes until restart. Invalidating for a
// transaction that later rolls back is harmless: the lazy reload just
// re-reads the committed state.
func (s *DoltStore) invalidateConfigCaches(key string) {
	s.cacheMu.Lock()
	switch key {
	case "status.custom":
		s.customStatusCached = false
		s.customStatusCache = nil
		s.customStatusDetailedCache = nil
	case "types.custom":
		s.customTypeCached = false
		s.customTypeCache = nil
	case "types.infra":
		s.infraTypeCached = false
		s.infraTypeCache = nil
	}
	s.cacheMu.Unlock()
}

// GetConfig retrieves a configuration value
func (s *DoltStore) GetConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetConfigInTx(ctx, tx, key)
		return err
	})
	return value, err
}

// GetAllConfig retrieves all configuration values
func (s *DoltStore) GetAllConfig(ctx context.Context) (map[string]string, error) {
	var result map[string]string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.GetAllConfigInTx(ctx, tx)
		return err
	})
	return result, err
}

// DeleteConfig removes a configuration value
//
// Like SetConfig, a nil return means the deletion is in HEAD (or deferred to
// the caller's commit point under issueops.WithDeferredVersionCommit).
func (s *DoltStore) DeleteConfig(ctx context.Context, key string) error {
	if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.DeleteConfigInTx(ctx, tx, key)
	}); err != nil {
		return err
	}
	return s.publishConfigWrite(ctx, configWriteTables(), "bd: config unset "+key)
}

// SetMetadata sets a metadata value
func (s *DoltStore) SetMetadata(ctx context.Context, key, value string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.SetMetadataInTx(ctx, tx, key, value)
	})
}

// GetMetadata retrieves a metadata value
func (s *DoltStore) GetMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetMetadataInTx(ctx, tx, key)
		return err
	})
	return value, err
}

// SetLocalMetadata sets a value in the dolt-ignored local_metadata table.
// Used for clone-local state that should not generate merge conflicts.
func (s *DoltStore) SetLocalMetadata(ctx context.Context, key, value string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.SetLocalMetadataInTx(ctx, tx, key, value)
	})
}

// GetLocalMetadata retrieves a value from the dolt-ignored local_metadata table.
// Returns ("", nil) if the key does not exist.
func (s *DoltStore) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = issueops.GetLocalMetadataInTx(ctx, tx, key)
		return err
	})
	return value, err
}

func (s *DoltStore) loadCustomConfigCache(ctx context.Context) {
	s.cacheMu.Lock()
	if s.customStatusCached && s.customTypeCached {
		s.cacheMu.Unlock()
		return
	}
	s.cacheMu.Unlock()

	var statuses []types.CustomStatus
	var customTypes []string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var resolveErr error
		statuses, customTypes, resolveErr = issueops.ResolveCustomConfigInTx(ctx, tx)
		return resolveErr
	})
	if err != nil {
		log.Printf("warning: failed to resolve custom config: %v", err)
		if yamlStatuses := config.GetCustomStatusesFromYAML(); len(yamlStatuses) > 0 {
			statuses = issueops.ParseStatusFallback(yamlStatuses)
		}
		if yamlTypes := config.GetCustomTypesFromYAML(); len(yamlTypes) > 0 {
			customTypes = yamlTypes
		}
	}

	s.cacheMu.Lock()
	if !s.customStatusCached {
		s.customStatusDetailedCache = statuses
		s.customStatusCache = types.CustomStatusNames(statuses)
		s.customStatusCached = true
	}
	if !s.customTypeCached {
		s.customTypeCache = customTypes
		s.customTypeCached = true
	}
	s.cacheMu.Unlock()
}

func (s *DoltStore) GetCustomStatuses(ctx context.Context) ([]string, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customStatusCache, nil
}

func (s *DoltStore) GetCustomStatusesDetailed(ctx context.Context) ([]types.CustomStatus, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customStatusDetailedCache, nil
}

// GetCustomTypes returns custom issue type values from config.
// If the database doesn't have custom types configured, falls back to config.yaml.
// Returns an empty slice if no custom types are configured.
// Results are cached per DoltStore lifetime and invalidated when SetConfig
// updates the "types.custom" key.
func (s *DoltStore) GetCustomTypes(ctx context.Context) ([]string, error) {
	s.loadCustomConfigCache(ctx)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.customTypeCache, nil
}

// GetInfraTypes returns infrastructure type names from config.
// Infrastructure types are routed to the wisps table to keep the versioned
// issues table clean. Defaults to ["agent", "role", "message"] if
// no custom configuration exists.
// Falls back: DB config "types.infra" → config.yaml types.infra → defaults.
// Results are cached per DoltStore lifetime and invalidated when SetConfig
// updates the "types.infra" key.
func (s *DoltStore) GetInfraTypes(ctx context.Context) map[string]bool {
	s.cacheMu.Lock()
	if s.infraTypeCached {
		result := s.infraTypeCache
		s.cacheMu.Unlock()
		return result
	}
	s.cacheMu.Unlock()

	var result map[string]bool
	if err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		result = issueops.ResolveInfraTypesInTx(ctx, tx)
		return nil
	}); err != nil || result == nil {
		// DB unavailable — fall back to YAML then defaults.
		var typeList []string
		if yamlTypes := config.GetInfraTypesFromYAML(); len(yamlTypes) > 0 {
			typeList = yamlTypes
		} else {
			typeList = domain.DefaultInfraTypes()
		}
		result = make(map[string]bool, len(typeList))
		for _, t := range typeList {
			result[t] = true
		}
	}

	s.cacheMu.Lock()
	s.infraTypeCache = result
	s.infraTypeCached = true
	s.cacheMu.Unlock()

	return result
}
