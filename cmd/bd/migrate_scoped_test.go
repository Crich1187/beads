package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/scopedbundle"
)

func TestMigrateScopedCommandContractHasNoCredentialOrSchemaFlags(t *testing.T) {
	if migrateScopedCmd.Parent() != migrateCmd {
		t.Fatal("migrate scoped is not registered below migrate")
	}
	for _, command := range migrateScopedCmd.Commands() {
		if !isScopedMigrateCommand(command) {
			t.Fatalf("%s does not bypass normal store open", command.CommandPath())
		}
		forbidden := []string{"password", "token", "dsn", "host", "port", "database", "migrate", "force"}
		for _, name := range forbidden {
			if command.Flags().Lookup(name) != nil {
				t.Fatalf("%s exposes forbidden flag --%s", command.CommandPath(), name)
			}
		}
	}
}

func TestMigrateScopedBundleOutputIs0600AndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.json")
	bundle := scopedbundle.Bundle{}
	if err := writeScopedBundle(path, &bundle); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("bundle mode = %04o want 0600", got)
	}
	if err := writeScopedBundle(path, &bundle); err == nil {
		t.Fatal("second write overwrote immutable bundle path")
	}
}

func TestDecodeScopedMappingRejectsUnknownFields(t *testing.T) {
	_, err := decodeScopedMapping(strings.NewReader(`{
		"version":1,"expected_count":1,"source_prefix":"source-","target_prefix":"target-",
		"pairs":[{"source":"source-1","target":"target-1"}],"sha256":"invalid",
		"automatic_scope_expansion":true
	}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown mapping field error = %v", err)
	}
}
