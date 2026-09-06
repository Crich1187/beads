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

func TestMigrateScopedApplyBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	bundlePath := filepath.Join(dir, "invalid-bundle.json")
	if err := os.WriteFile(bundlePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	freezeTown(t, dir, "scoped-test-operator", "scoped apply freeze test")

	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir,
		"migrate", "scoped", "apply",
		"--bundle", bundlePath,
		"--expect-current", strings.Repeat("0", 64),
		"--actor", "scoped-test-actor")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "frozen for migration") {
		t.Fatalf("stderr missing migration-freeze refusal:\n%s", stderr)
	}
	if strings.Contains(stderr, "unsupported bundle") {
		t.Fatalf("bundle was read before migration-freeze refusal:\n%s", stderr)
	}
}
