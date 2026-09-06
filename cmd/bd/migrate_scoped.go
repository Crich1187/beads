package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/eventsjournal"
	"github.com/steveyegge/beads/internal/scopedbundle"
	"github.com/steveyegge/beads/internal/storage/doltutil"
)

var migrateScopedCmd = &cobra.Command{
	Use:   "scoped",
	Short: "Export, inspect, or atomically apply an exact reviewed row bundle",
	Long: `Operate on an explicit, digest-bound issue mapping without opening the
normal Beads store or running schema migrations. The active workspace selects
the configured Dolt database; this command has no server or credential flags.`,
}

var migrateScopedInspectCmd = &cobra.Command{
	Use:          "inspect",
	Short:        "Fingerprint the exact mapped source or target state",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		mapPath, _ := cmd.Flags().GetString("map")
		sideValue, _ := cmd.Flags().GetString("id-side")
		if mapPath == "" {
			return fmt.Errorf("--map is required")
		}
		side := scopedbundle.IDSide(sideValue)
		if side != scopedbundle.SourceSide && side != scopedbundle.TargetSide {
			return fmt.Errorf("--id-side must be source or target")
		}
		mapping, err := readScopedMapping(mapPath)
		if err != nil {
			return err
		}
		db, cleanup, err := openScopedBundleConnection(rootCtx)
		if err != nil {
			return err
		}
		defer cleanup()
		state, err := scopedbundle.Inspect(rootCtx, db, mapping, side)
		if err != nil {
			return err
		}
		return outputJSON(scopedStateOutput(state, mapping, string(side)))
	},
}

var migrateScopedExportCmd = &cobra.Command{
	Use:          "export",
	Short:        "Write a deterministic bundle for an exact reviewed source set",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		mapPath, _ := cmd.Flags().GetString("map")
		outputPath, _ := cmd.Flags().GetString("output")
		if mapPath == "" || outputPath == "" {
			return fmt.Errorf("--map and --output are required")
		}
		mapping, err := readScopedMapping(mapPath)
		if err != nil {
			return err
		}
		db, cleanup, err := openScopedBundleConnection(rootCtx)
		if err != nil {
			return err
		}
		defer cleanup()
		bundle, err := scopedbundle.Export(rootCtx, db, mapping)
		if err != nil {
			return err
		}
		if err := writeScopedBundle(outputPath, bundle); err != nil {
			return err
		}
		return outputJSON(map[string]any{
			"status":              "exported",
			"output":              outputPath,
			"bundle_sha256":       bundle.SHA256,
			"source_state_sha256": bundle.SourceStateSHA256,
			"mapping_sha256":      bundle.Mapping.SHA256,
			"issue_count":         bundle.Mapping.ExpectedCount,
		})
	},
}

var migrateScopedApplyCmd = &cobra.Command{
	Use:          "apply",
	Short:        "Atomically apply a bundle against an exact current-state digest",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		bundlePath, _ := cmd.Flags().GetString("bundle")
		expected, _ := cmd.Flags().GetString("expect-current")
		actor, _ := cmd.Flags().GetString("actor")
		if bundlePath == "" || expected == "" || actor == "" {
			return fmt.Errorf("--bundle, --expect-current, and --actor are required")
		}
		CheckReadonly("migrate scoped apply")
		bundle, err := readScopedBundle(bundlePath)
		if err != nil {
			return err
		}
		db, cleanup, err := openScopedBundleConnection(rootCtx)
		if err != nil {
			return err
		}
		defer cleanup()
		beadsDir := selectedDoltBeadsDir()
		result, err := scopedbundle.Apply(rootCtx, db, bundle, scopedbundle.ApplyOptions{
			ExpectedCurrentSHA256: expected,
			Actor:                 actor,
			JournalEnabled:        eventsjournal.EnabledFor(beadsDir),
		})
		if err != nil {
			return err
		}
		return outputJSON(map[string]any{
			"status":        "applied",
			"changed":       result.Changed,
			"before_sha256": result.BeforeSHA256,
			"after_sha256":  result.AfterSHA256,
			"bundle_sha256": bundle.SHA256,
		})
	},
}

func init() {
	migrateScopedInspectCmd.Flags().String("map", "", "Path to the reviewed mapping JSON")
	migrateScopedInspectCmd.Flags().String("id-side", "", "Mapping side to inspect: source or target")
	migrateScopedExportCmd.Flags().String("map", "", "Path to the reviewed mapping JSON")
	migrateScopedExportCmd.Flags().String("output", "", "New bundle JSON path (must not already exist)")
	migrateScopedApplyCmd.Flags().String("bundle", "", "Path to a verified scoped bundle JSON")
	migrateScopedApplyCmd.Flags().String("expect-current", "", "Exact current target state SHA-256")
	migrateScopedApplyCmd.Flags().String("actor", "", "Auditable applying identity")
	migrateScopedCmd.AddCommand(migrateScopedInspectCmd, migrateScopedExportCmd, migrateScopedApplyCmd)
	migrateCmd.AddCommand(migrateScopedCmd)
}

func isScopedMigrateCommand(command *cobra.Command) bool {
	for current := command; current != nil; current = current.Parent() {
		if current.Name() == "scoped" && current.Parent() != nil && current.Parent().Name() == "migrate" {
			return true
		}
	}
	return false
}

func readScopedMapping(path string) (scopedbundle.Mapping, error) {
	// #nosec G304 -- opening the operator-selected --map path is the command contract; strict JSON decoding and Mapping.Validate reject malformed content.
	file, err := os.Open(path)
	if err != nil {
		return scopedbundle.Mapping{}, fmt.Errorf("open mapping: %w", err)
	}
	defer file.Close()
	mapping, err := decodeScopedMapping(file)
	if err != nil {
		return scopedbundle.Mapping{}, fmt.Errorf("decode mapping: %w", err)
	}
	if err := mapping.Validate(); err != nil {
		return scopedbundle.Mapping{}, err
	}
	return mapping, nil
}

func decodeScopedMapping(reader io.Reader) (scopedbundle.Mapping, error) {
	var mapping scopedbundle.Mapping
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mapping); err != nil {
		return scopedbundle.Mapping{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return scopedbundle.Mapping{}, err
	}
	return mapping, nil
}

func readScopedBundle(path string) (scopedbundle.Bundle, error) {
	// #nosec G304 -- opening the operator-selected --bundle path is the command contract; strict JSON decoding and Bundle.Verify enforce its seal.
	file, err := os.Open(path)
	if err != nil {
		return scopedbundle.Bundle{}, fmt.Errorf("open bundle: %w", err)
	}
	defer file.Close()
	var bundle scopedbundle.Bundle
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return scopedbundle.Bundle{}, fmt.Errorf("decode bundle: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return scopedbundle.Bundle{}, err
	}
	if err := bundle.Verify(); err != nil {
		return scopedbundle.Bundle{}, err
	}
	return bundle, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return fmt.Errorf("read trailing JSON: %w", err)
	}
	return nil
}

func writeScopedBundle(path string, bundle *scopedbundle.Bundle) error {
	// #nosec G304 -- creating the operator-selected --output path is the command contract; O_EXCL refuses overwrite/symlink targets and mode 0600 limits access.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(bundle); err != nil {
		return fmt.Errorf("encode bundle: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync bundle: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	removeOnError = false
	return nil
}

func openScopedBundleConnection(ctx context.Context) (*sql.DB, func(), error) {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return nil, nil, HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}
	cfg, err := loadDoltBackendConfig(beadsDir)
	if err != nil {
		return nil, nil, err
	}
	dsn := doltutil.ServerDSN{
		Host:     cfg.GetDoltServerHost(),
		Port:     doltserver.DefaultConfig(beadsDir).Port,
		User:     cfg.GetDoltServerUser(),
		Password: os.Getenv("BEADS_DOLT_PASSWORD"),
		Database: cfg.GetDoltDatabase(),
		TLS:      cfg.GetDoltServerTLS(),
	}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open configured Dolt database: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("connect to configured Dolt database: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

func scopedStateOutput(state scopedbundle.State, mapping scopedbundle.Mapping, side string) map[string]any {
	counts := make(map[string]int, len(state.Tables))
	for _, table := range state.Tables {
		counts[table.Name] = len(table.Rows)
	}
	return map[string]any{
		"status":           "inspected",
		"id_side":          side,
		"schema_version":   state.Schema.Version,
		"state_sha256":     state.SHA256,
		"mapping_sha256":   mapping.SHA256,
		"expected_count":   mapping.ExpectedCount,
		"table_row_counts": counts,
	}
}
