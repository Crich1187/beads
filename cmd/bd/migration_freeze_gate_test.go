// Tests for the migration freeze write gate (dc-6jaq): bd write commands must
// refuse to run, with exit code ExitMigrationFrozen, while a MIGRATION-FREEZE
// marker sits in the working directory or any ancestor of it — or at the path
// named by BD_MIGRATION_FREEZE_FILE, which is authoritative when set.
//
// This file MUST NOT carry a cgo build tag: it exercises the default sqlite
// backend via a bd binary built with the gms_pure_go tag (mirrors
// update_multi_id_exit_test.go's convention).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/migration"
)

// vendorNeutralBans are substrings the refusal must never print. The gate used
// to key on a gastown town root and tell users to run `gt migrate thaw`, a CLI
// that does not ship with bd; nothing about the OSS binary's write refusal may
// name another vendor's product again.
var vendorNeutralBans = []string{"gt ", "gt migrate", "thaw", "town", "mayor", "GT_"}

func assertVendorNeutral(t *testing.T, stderr string) {
	t.Helper()
	for _, banned := range vendorNeutralBans {
		if strings.Contains(stderr, banned) {
			t.Errorf("refusal contains vendor-coupled substring %q:\n%s", banned, stderr)
		}
	}
}

// migrationFreezeEnv returns a hermetic environment for bd subprocess runs:
// no inherited BEADS_*/GT_* variables, HOME pinned to the test dir, metrics and
// daemons disabled.
//
// The freeze override is pinned to a path that cannot exist, not merely
// stripped. Because the gate walks every ancestor to the filesystem root, a
// stray MIGRATION-FREEZE anywhere above TMPDIR would otherwise fail these
// tests at `bd init` — and the pin is authoritative, so it also holds the walk
// off while a test is setting its workspace up. Tests that actually exercise
// the walk layer freezeWalkEnv on top. (TestMain pins the same variable for
// the whole package, since the walk makes every write-subprocess suite here
// equally sensitive to an ambient marker; this is the belt to that's braces.)
func migrationFreezeEnv(dir string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BEADS_") || strings.HasPrefix(e, "GT_") {
			continue
		}
		if strings.HasPrefix(e, migration.EnvFreezeFile+"=") {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"HOME="+dir,
		"USERPROFILE="+dir,
		"BD_NON_INTERACTIVE=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"BEADS_NO_DAEMON=1",
		"BEADS_DOLT_AUTO_START=0",
		migration.EnvFreezeFile+"="+filepath.Join(dir, "no-such-freeze-marker"),
	)
}

// freezeWalkEnv clears the override so the subprocess uses the ancestor walk.
func freezeWalkEnv() []string {
	return []string{migration.EnvFreezeFile + "="}
}

// runBDFrozen runs bd with the ancestor walk enabled — the mode every
// marker-on-disk test needs.
func runBDFrozen(t *testing.T, bd, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runBDMigrationFreezeWithEnv(t, bd, dir, freezeWalkEnv(), args...)
}

// runBDMigrationFreeze runs the bd binary and returns stdout, stderr, and the
// exit code. Only a failure to launch the process fails the test; nonzero
// exits are returned to the caller for assertion.
func runBDMigrationFreeze(t *testing.T, bd, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runBDMigrationFreezeWithEnv(t, bd, dir, nil, args...)
}

// runBDMigrationFreezeWithEnv is runBDMigrationFreeze plus caller-supplied
// environment variables layered on top of the hermetic base env (e.g.
// BD_DEBUG=1, for tests that need to observe debug.Logf output).
func runBDMigrationFreezeWithEnv(t *testing.T, bd, dir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(migrationFreezeEnv(dir), extraEnv...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("bd %v did not run: %v", args, err)
		}
		return outBuf.String(), errBuf.String(), ee.ExitCode()
	}
	return outBuf.String(), errBuf.String(), 0
}

// setupMigrationFreezeWorkspaceIn builds bd and initializes a fresh sqlite-
// backed database in dir. The workspace needs nothing but itself: the gate
// keys on the freeze marker file, not on any orchestrator structure around it.
func setupMigrationFreezeWorkspaceIn(t *testing.T, dir string) (bd string) {
	t.Helper()
	bd = buildBDForInitTests(t)
	runGitForBootstrapTest(t, dir, "init", "-q")
	runGitForBootstrapTest(t, dir, "config", "core.hooksPath", ".git/hooks")

	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir,
		"init", "--prefix", "test", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents")
	if code != 0 {
		t.Fatalf("bd init failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	return bd
}

func setupMigrationFreezeWorkspace(t *testing.T) (bd, dir string) {
	t.Helper()
	dir = t.TempDir()
	return setupMigrationFreezeWorkspaceIn(t, dir), dir
}

// writeFreezeMarker writes a MIGRATION-FREEZE marker in dir and returns its path.
func writeFreezeMarker(t *testing.T, dir, operator, reason string) string {
	t.Helper()
	path := filepath.Join(dir, migration.FileName)
	content := operator + "\t2026-08-16T12:00:00Z\t" + reason + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestCreateBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	stdout, stderr, code := runBDFrozen(t, bd, dir, "create", "should not be created", "-p", "2")

	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
	}
	for _, want := range []string{
		"workspace is frozen for migration",
		"migrator",
		"dolt v2 migration",
		// The marker's real path, not just its name: that is what makes
		// "remove that file" actionable. Matched from the workspace
		// directory's own name down, so a symlinked temp root (macOS
		// /var → /private/var) doesn't fail the assertion.
		filepath.Join(filepath.Base(dir), migration.FileName),
		"To resume writes, remove that file",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	assertVendorNeutral(t, stderr)
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty when blocked, got:\n%s", stdout)
	}
}

func TestUpdateBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)

	// Create the issue BEFORE freezing — create itself isn't under test here.
	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir, "create", "pre-freeze issue", "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("setup bd create failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &issue); err != nil || issue.ID == "" {
		t.Fatalf("parsing create --json output: %v\n%s", err, stdout)
	}

	writeFreezeMarker(t, dir, "athos", "server migration in progress")

	stdout, stderr, code = runBDFrozen(t, bd, dir, "update", issue.ID, "-p", "1")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
	}
	if !strings.Contains(stderr, "workspace is frozen for migration") {
		t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
	}
}

// TestWriteBlockedByAncestorFreezeMarker is the vendor-neutrality change made
// observable: the marker sits in the workspace's PARENT, with no orchestrator
// structure anywhere, and still freezes writes. That is exactly the shape of a
// marker written above a set of sibling repos.
func TestWriteBlockedByAncestorFreezeMarker(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("creating %s: %v", work, err)
	}
	bd := setupMigrationFreezeWorkspaceIn(t, work)
	writeFreezeMarker(t, root, "migrator", "tree-wide migration")

	stdout, stderr, code := runBDFrozen(t, bd, work, "create", "should not be created", "-p", "2")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d (marker one level above the workspace)\nstdout:\n%s\nstderr:\n%s",
			code, ExitMigrationFrozen, stdout, stderr)
	}
	if !strings.Contains(stderr, "workspace is frozen for migration") {
		t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
	}
	assertVendorNeutral(t, stderr)
}

// TestMigrationFreezeFileEnvOverride pins both directions of
// BD_MIGRATION_FREEZE_FILE: set to an existing file it freezes a workspace
// with no marker anywhere above it, and set to a missing path it does NOT
// freeze even with a marker right there — that second half is what makes the
// variable a usable opt-out.
func TestMigrationFreezeFileEnvOverride(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	elsewhere := t.TempDir()
	override := filepath.Join(elsewhere, "freeze-marker")
	if err := os.WriteFile(override, []byte("migrator\t2026-08-16T12:00:00Z\tout-of-tree freeze\n"), 0644); err != nil {
		t.Fatalf("writing override marker: %v", err)
	}

	t.Run("existing override file blocks with no marker in the tree", func(t *testing.T) {
		stdout, stderr, code := runBDMigrationFreezeWithEnv(t, bd, dir,
			[]string{migration.EnvFreezeFile + "=" + override},
			"create", "should not be created", "-p", "2")

		if code != ExitMigrationFrozen {
			t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
		}
		for _, want := range []string{
			"workspace is frozen for migration",
			"out-of-tree freeze",
			override,
			"or unset " + migration.EnvFreezeFile,
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr)
			}
		}
		assertVendorNeutral(t, stderr)
	})

	t.Run("missing override file wins over a marker in the workspace", func(t *testing.T) {
		writeFreezeMarker(t, dir, "migrator", "should be ignored")
		t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, migration.FileName)) })

		stdout, stderr, code := runBDMigrationFreezeWithEnv(t, bd, dir,
			[]string{migration.EnvFreezeFile + "=" + filepath.Join(elsewhere, "no-such-marker")},
			"create", "normal issue", "-p", "2", "--json")

		if code != 0 {
			t.Fatalf("exit code = %d, want 0 — the override must skip the ancestor walk entirely\nstdout:\n%s\nstderr:\n%s",
				code, stdout, stderr)
		}
	})
}

// TestFreezeKeysOnTargetWorkspaceNotCwd is the core-contract test: the gate
// must follow the store being written, not the shell the caller happens to be
// sitting in. Both shapes here target a frozen workspace from an unrelated
// working directory, and neither one chdirs — `bd -C` only sets BEADS_DIR.
// Before this, both walked the caller's ancestry, found nothing, and wrote
// straight through the freeze.
func TestFreezeKeysOnTargetWorkspaceNotCwd(t *testing.T) {
	frozenRoot := t.TempDir()
	work := filepath.Join(frozenRoot, "repo")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("creating %s: %v", work, err)
	}
	bd := setupMigrationFreezeWorkspaceIn(t, work)
	writeFreezeMarker(t, frozenRoot, "migrator", "cross-tree migration")

	// The caller's cwd is nowhere near the frozen tree.
	outside := t.TempDir()

	t.Run("BEADS_DIR", func(t *testing.T) {
		stdout, stderr, code := runBDMigrationFreezeWithEnv(t, bd, outside,
			append(freezeWalkEnv(), "BEADS_DIR="+filepath.Join(work, ".beads")),
			"create", "should not be created", "-p", "2")
		if code != ExitMigrationFrozen {
			t.Fatalf("exit code = %d, want %d — BEADS_DIR named a frozen workspace\nstdout:\n%s\nstderr:\n%s",
				code, ExitMigrationFrozen, stdout, stderr)
		}
		if !strings.Contains(stderr, "workspace is frozen for migration") {
			t.Errorf("stderr missing the refusal:\n%s", stderr)
		}
	})

	t.Run("-C", func(t *testing.T) {
		stdout, stderr, code := runBDFrozen(t, bd, outside,
			"-C", work, "create", "should not be created", "-p", "2")
		if code != ExitMigrationFrozen {
			t.Fatalf("exit code = %d, want %d — -C named a frozen workspace\nstdout:\n%s\nstderr:\n%s",
				code, ExitMigrationFrozen, stdout, stderr)
		}
		if !strings.Contains(stderr, "workspace is frozen for migration") {
			t.Errorf("stderr missing the refusal:\n%s", stderr)
		}
	})
}

// TestInitReinitBlockedDuringMigrationFreeze covers the skip-store bypass, the
// sharpest hole of the lot: `init` is in noDbCommands, so PersistentPreRunE
// returns before its freeze gate, and init.go calls neither CheckReadonly nor
// the gate. `bd init --reinit-local` therefore destroyed and recreated the
// very database the marker was protecting — permanently, exit 0, marker never
// consulted.
func TestInitReinitBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)

	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir, "create", "issue that must survive", "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("setup bd create failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &issue); err != nil || issue.ID == "" {
		t.Fatalf("parsing create --json output: %v\n%s", err, stdout)
	}

	writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	stdout, stderr, code = runBDFrozen(t, bd, dir,
		"init", "--prefix", "test", "--reinit-local", "--non-interactive", "--quiet", "--skip-hooks", "--skip-agents")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d — a destructive reinit must not run during a freeze\nstdout:\n%s\nstderr:\n%s",
			code, ExitMigrationFrozen, stdout, stderr)
	}
	if !strings.Contains(stderr, "workspace is frozen for migration") {
		t.Errorf("stderr missing the refusal:\n%s", stderr)
	}
	assertVendorNeutral(t, stderr)

	// The database must still be there, with its issue in it.
	if err := os.Remove(filepath.Join(dir, migration.FileName)); err != nil {
		t.Fatalf("removing freeze marker: %v", err)
	}
	stdout, stderr, code = runBDMigrationFreeze(t, bd, dir, "show", issue.ID, "--json")
	if code != 0 {
		t.Fatalf("bd show after the refused reinit failed (exit %d) — the data was destroyed anyway:\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
}

// TestFreezeRefusalHasNoUsageDump: the refusal is returned as an error now, so
// cobra renders it — and on a command that sets neither SilenceErrors nor
// SilenceUsage (duplicate, supersede, dep relate/unrelate, backup *, ado sync,
// migrate-personal, batch) that means "Error: exit code 14" plus a full usage
// block after the clean refusal, making it read like a syntax error. Every
// other freeze test uses a silenced command, so nothing else catches this.
func TestFreezeRefusalHasNoUsageDump(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "duplicate", args: []string{"duplicate", "test-aaaa", "--of", "test-bbbb"}},
		{name: "relate", args: []string{"dep", "relate", "test-aaaa", "test-bbbb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runBDFrozen(t, bd, dir, tc.args...)
			if code != ExitMigrationFrozen {
				t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
			}
			if !strings.Contains(stderr, "workspace is frozen for migration") {
				t.Fatalf("stderr missing the refusal:\n%s", stderr)
			}
			if strings.Contains(stderr, "exit code 14") {
				t.Errorf("cobra printed the exitError placeholder after the refusal:\n%s", stderr)
			}
			if strings.Contains(stderr, "Usage:") || strings.Contains(stdout, "Usage:") {
				t.Errorf("cobra dumped usage after a clean refusal — it reads as a syntax error:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		})
	}
}

// TestCreateNotBlockedWithoutFreeze is the regression-safety check: normal bd
// usage (no freeze marker present, the overwhelming common case) must be
// completely unaffected by this gate.
func TestCreateNotBlockedWithoutFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)

	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir, "create", "normal issue", "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("bd create failed (exit %d) with no freeze marker present:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &issue); err != nil {
		t.Fatalf("parsing create --json output: %v\n%s", err, stdout)
	}
	if issue.ID == "" {
		t.Fatalf("bd create --json returned no id:\n%s", stdout)
	}
}

// TestWorkspaceUsableAfterMigrationFreezeBlockedWrite is the end-to-end half of the
// defer-skip fix (see TestMigrationFreezeErrorReturnsInsteadOfExiting for the
// direct proof). The root PersistentPreRunE takes the workspace and
// physical-root gates and closes the store before releasing them in a defer;
// the freeze refusal used to os.Exit straight through that defer. A workspace
// must be immediately usable after a blocked write.
func TestWorkspaceUsableAfterMigrationFreezeBlockedWrite(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	marker := writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	stdout, stderr, code := runBDFrozen(t, bd, dir, "create", "should not be created", "-p", "2")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("removing freeze marker: %v", err)
	}

	stdout, stderr, code = runBDMigrationFreeze(t, bd, dir, "create", "post-thaw issue", "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("bd create after the freeze was lifted failed (exit %d) — the blocked run left the workspace wedged:\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
}

// TestMigrationFreezeErrorReturnsInsteadOfExiting is the direct regression
// test for the defer-skip bug: migrationFreezeError must RETURN its refusal so
// callers that hold live cleanup (the root PersistentPreRunE closes the store
// and releases the workspace gates from a defer; runImport finalizes its
// metrics event) still run it. If the function ever goes back to os.Exit, this
// test binary dies mid-run and the whole package fails — which is the point.
func TestMigrationFreezeErrorReturnsInsteadOfExiting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, migration.FileName),
		[]byte("migrator\t2026-08-16T12:00:00Z\tschema move\n"), 0644); err != nil {
		t.Fatalf("writing freeze marker: %v", err)
	}
	t.Chdir(dir)
	t.Setenv(migration.EnvFreezeFile, "") // unset: exercise the ancestor walk

	// The shape of the PersistentPreRunE call site: a named return with a
	// deferred cleanup that only fires on the error path.
	cleanupRan := false
	var gateErr error
	stderr := captureStderr(t, func() {
		gateErr = func() (retErr error) {
			defer func() {
				if retErr != nil {
					cleanupRan = true
				}
			}()
			return migrationFreezeError("create")
		}()
	})

	if gateErr == nil {
		t.Fatalf("migrationFreezeError returned nil with a freeze marker in the working directory")
	}
	if !cleanupRan {
		t.Errorf("the caller's deferred cleanup did not run — the refusal must be returned, not exited")
	}
	code, ok := exitCodeFromError(gateErr)
	if !ok || code != ExitMigrationFrozen {
		t.Errorf("exitCodeFromError = (%d, %v), want (%d, true)", code, ok, ExitMigrationFrozen)
	}
	assertVendorNeutral(t, stderr)
}

// TestMigrationFreezeErrorEmptyMarkerNamesNoOperator covers the `touch
// MIGRATION-FREEZE` case: with no operator recorded, the refusal must say
// nothing rather than print an empty "(by )".
func TestMigrationFreezeErrorEmptyMarkerNamesNoOperator(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, migration.FileName), nil, 0644); err != nil {
		t.Fatalf("writing empty freeze marker: %v", err)
	}
	t.Chdir(dir)
	t.Setenv(migration.EnvFreezeFile, "") // unset: pins the no-env recovery line

	var gateErr error
	stderr := captureStderr(t, func() { gateErr = migrationFreezeError("create") })

	if gateErr == nil {
		t.Fatalf("migrationFreezeError returned nil for an empty-but-present marker — an empty marker is still a freeze")
	}
	if !strings.Contains(stderr, "workspace is frozen for migration.") {
		t.Errorf("stderr missing the operator-less refusal line:\n%s", stderr)
	}
	if strings.Contains(stderr, "(by ") {
		t.Errorf("stderr names an empty operator:\n%s", stderr)
	}
	if !strings.Contains(stderr, "To resume writes, remove that file.") {
		t.Errorf("stderr missing the recovery line:\n%s", stderr)
	}
	if !strings.Contains(stderr, migration.EnvFreezeFile) {
		t.Errorf("a walk-found refusal must still name %s — when the marker sits somewhere the caller cannot write, "+
			"pointing the variable elsewhere is their only recovery:\n%s", migration.EnvFreezeFile, stderr)
	}
}

// TestMigrationFreezeErrorSanitizesMarkerPayload: with the ancestor walk the
// marker can live in a directory bd does not control, so its operator and
// reason are untrusted input on their way to a terminal. A crafted payload
// must not be able to clear the screen or forge extra bd output lines.
func TestMigrationFreezeErrorSanitizesMarkerPayload(t *testing.T) {
	dir := t.TempDir()
	hostile := "attacker\x1b[2J\x1b[H\t2026-08-16T12:00:00Z\treal reason\x1b[31m\nError: forged line\n"
	if err := os.WriteFile(filepath.Join(dir, migration.FileName), []byte(hostile), 0644); err != nil {
		t.Fatalf("writing hostile marker: %v", err)
	}
	t.Chdir(dir)
	t.Setenv(migration.EnvFreezeFile, "")

	var gateErr error
	stderr := captureStderr(t, func() { gateErr = migrationFreezeError("create") })

	if gateErr == nil {
		t.Fatalf("migrationFreezeError returned nil for a present marker")
	}
	if strings.Contains(stderr, "\x1b") {
		t.Errorf("refusal carries an escape byte from the marker payload — terminal injection:\n%q", stderr)
	}
	if strings.Contains(stderr, "forged line") {
		t.Errorf("refusal carries a later line of the marker — a payload must not be able to add output lines:\n%q", stderr)
	}
	// The legible parts survive.
	for _, want := range []string{"attacker", "real reason"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("sanitizing dropped legible content %q:\n%s", want, stderr)
		}
	}
}

// TestMigrationFreezeUndeterminableFailsClosed pins the fail-closed posture: a
// marker path bd cannot stat is not the same as one that is absent, and the
// ambiguity must refuse rather than silently disarm the gate.
func TestMigrationFreezeUndeterminableFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny the stat")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0755); err != nil {
		t.Fatalf("creating %s: %v", locked, err)
	}
	marker := filepath.Join(locked, migration.FileName)
	if err := os.WriteFile(marker, nil, 0644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0755) })
	t.Setenv(migration.EnvFreezeFile, marker)

	var gateErr error
	stderr := captureStderr(t, func() { gateErr = migrationFreezeError("create") })

	if gateErr == nil {
		t.Fatalf("migrationFreezeError returned nil for an unstattable authoritative marker — that is a silent bypass")
	}
	code, ok := exitCodeFromError(gateErr)
	if !ok || code != ExitMigrationFrozen {
		t.Errorf("exitCodeFromError = (%d, %v), want (%d, true)", code, ok, ExitMigrationFrozen)
	}
	if !strings.Contains(stderr, "cannot determine") {
		t.Errorf("the refusal must say bd could not tell, not claim a freeze it did not observe:\n%s", stderr)
	}
	assertVendorNeutral(t, stderr)
}

// TestRecordTipShownSkippedDuringFreeze covers the PostRunE maintenance half of
// the freeze promise at its one immediate-write site: with dolt auto-commit
// off, recordTipShown writes tip_*_last_shown to the store the moment a tip is
// displayed rather than deferring to PersistentPostRunE, so the guard there
// does not cover it. Showing a tip must never write into a frozen workspace.
func TestRecordTipShownSkippedDuringFreeze(t *testing.T) {
	saved := commandFreeze
	t.Cleanup(func() { commandFreeze = saved })

	writer := &fakeTipMetadataWriter{}
	commandFreeze = migration.Result{Path: filepath.Join(t.TempDir(), migration.FileName)}
	recordTipShown(writer, "some-tip")
	if writer.calls != 0 {
		t.Errorf("recordTipShown wrote %d metadata keys during a freeze, want 0", writer.calls)
	}
	if commandDidWriteTipMetadata {
		t.Errorf("recordTipShown recorded a pending tip write during a freeze")
	}

	commandFreeze = migration.Result{}
	recordTipShown(writer, "some-tip")
	if writer.calls == 0 {
		t.Errorf("recordTipShown wrote nothing with no freeze active — the guard must not be unconditional")
	}
}

type fakeTipMetadataWriter struct{ calls int }

func (f *fakeTipMetadataWriter) SetLocalMetadata(_ context.Context, _, _ string) error {
	f.calls++
	return nil
}

// TestQuickBlockedDuringMigrationFreeze regression-checks the sharpest gap
// flagged in review of the original gate (dc-6jaq, PR #5826): the gate
// hand-picked five commands (create, update, close, remember, import), but
// "bd q" (quick.go) is create's own documented shorthand and called
// CheckReadonly directly rather than going through create's RunE — so it
// was never gated even though create was. Now that CheckReadonly itself
// folds in the freeze check, every one of its ~120 call sites is covered
// automatically, "q" included.
func TestQuickBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)
	writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	stdout, stderr, code := runBDFrozen(t, bd, dir, "q", "should not be created")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
	}
	if !strings.Contains(stderr, "workspace is frozen for migration") {
		t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty when blocked, got:\n%s", stdout)
	}
}

// TestLabelAddBlockedDuringMigrationFreeze checks a second, unrelated write
// command that was never part of the original hand-picked five either —
// evidence the fold-into-CheckReadonly fix covers the write surface
// generally, not just the one bypass ("bd q") review happened to name.
func TestLabelAddBlockedDuringMigrationFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)

	stdout, stderr, code := runBDMigrationFreeze(t, bd, dir, "create", "pre-freeze issue for label", "-p", "2", "--json")
	if code != 0 {
		t.Fatalf("setup bd create failed (exit %d):\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &issue); err != nil || issue.ID == "" {
		t.Fatalf("parsing create --json output: %v\n%s", err, stdout)
	}

	writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

	stdout, stderr, code = runBDFrozen(t, bd, dir, "label", "add", issue.ID, "should-not-be-added")
	if code != ExitMigrationFrozen {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
	}
	if !strings.Contains(stderr, "workspace is frozen for migration") {
		t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
	}
}

// TestImportBlockedDuringMigrationFreeze covers bd import both ways round.
//
// Plain `bd import` is stopped by the early gate in PersistentPreRunE, like
// any other write. The `--dry-run` row is the one that pins runImport's OWN
// call: a preview sets useReadOnly, so it skips the early gate, and runImport
// never calls CheckReadonly — delete the check from import.go and only this
// row goes red. It also pins the claim the PreRunE comment makes about
// previews being re-blocked at the per-command chokepoint, which for import is
// exactly that call and nothing else.
func TestImportBlockedDuringMigrationFreeze(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "import", args: []string{"import"}},
		{name: "import --dry-run", args: []string{"import", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd, dir := setupMigrationFreezeWorkspace(t)
			jsonlPath := filepath.Join(dir, "incoming.jsonl")
			if err := os.WriteFile(jsonlPath,
				[]byte(`{"id":"test-zzzz","title":"should not be imported","status":"open","priority":2,"issue_type":"task"}`+"\n"),
				0644); err != nil {
				t.Fatalf("writing import fixture: %v", err)
			}
			writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

			args := append(append([]string{}, tc.args...), jsonlPath)
			stdout, stderr, code := runBDFrozen(t, bd, dir, args...)
			if code != ExitMigrationFrozen {
				t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitMigrationFrozen, stdout, stderr)
			}
			if !strings.Contains(stderr, "workspace is frozen for migration") {
				t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
			}
			assertVendorNeutral(t, stderr)
		})
	}
}

// TestAutoMigrateSkippedDuringMigrationFreeze is the structural ordering
// check (dc-6jaq review, ask #2): a frozen write must be blocked before
// PersistentPreRunE's own store-touching side effects run, not after, from
// inside the write command's own RunE. autoMigrateOnVersionBump
// (version_tracking.go) opens its own store connection and can apply a real
// schema migration — the most dangerous write in this path — and ran
// unconditionally for every non-preview command before this fix, freeze or
// not.
//
// An old .local_version forces trackBdVersion to detect a version "bump" so
// autoMigrateOnVersionBump's body actually does something observable
// instead of short-circuiting on "no upgrade detected" — then BD_DEBUG=1
// surfaces its unconditional "auto-migrate:"-prefixed debug.Logf lines, so
// their absence is direct evidence the function was never entered.
//
// The "list" case is review round 2's ask #1: a command classified
// read-only must NOT be blocked (diagnosis has to keep working during a
// freeze — exit 0, no refusal), but the same two maintenance side effects
// must still be skipped, because they are this hook's own writes and run
// independently of the command's classification. Reproduced pre-fix:
// freeze the workspace, seed .local_version with a stale version, run
// `BD_DEBUG=1 bd list` — exit 0, but the "auto-migrate:" line appeared and
// .local_version was rewritten to the current version anyway.
func TestAutoMigrateSkippedDuringMigrationFreeze(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantExit    int
		wantBlocked bool
	}{
		{name: "create", args: []string{"create", "should not be created", "-p", "2"}, wantExit: ExitMigrationFrozen, wantBlocked: true},
		{name: "list", args: []string{"list"}, wantExit: 0, wantBlocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd, dir := setupMigrationFreezeWorkspace(t)

			localVersionPath := filepath.Join(dir, ".beads", localVersionFile)
			if err := os.WriteFile(localVersionPath, []byte("0.0.1\n"), 0644); err != nil {
				t.Fatalf("writing fake old %s: %v", localVersionFile, err)
			}

			writeFreezeMarker(t, dir, "migrator", "dolt v2 migration")

			stdout, stderr, code := runBDMigrationFreezeWithEnv(t, bd, dir, append(freezeWalkEnv(), "BD_DEBUG=1"), tt.args...)

			if code != tt.wantExit {
				t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.wantExit, stdout, stderr)
			}
			if tt.wantBlocked && !strings.Contains(stderr, "workspace is frozen for migration") {
				t.Errorf("stderr missing 'workspace is frozen for migration':\n%s", stderr)
			}
			if strings.Contains(stderr, "auto-migrate:") {
				t.Errorf("autoMigrateOnVersionBump ran its store-opening body during a freeze (found an "+
					"'auto-migrate:' debug log line in stderr) — the freeze check must skip it while "+
					"frozen regardless of whether the command itself is a read or a write:\n%s", stderr)
			}

			// The skip must be real, not just quiet: the frozen store's
			// .local_version must stay exactly as seeded, not get silently
			// rewritten to the running binary's version by trackBdVersion.
			got, err := os.ReadFile(localVersionPath)
			if err != nil {
				t.Fatalf("reading %s after run: %v", localVersionFile, err)
			}
			if strings.TrimSpace(string(got)) != "0.0.1" {
				t.Errorf("%s = %q after a frozen run, want unchanged \"0.0.1\" (trackBdVersion must not "+
					"write during a freeze)", localVersionFile, strings.TrimSpace(string(got)))
			}
		})
	}
}

// TestAutoMigrateStillRunsWithoutFreeze is the companion regression-safety
// check for the ask-#2 fix: without a freeze marker, the new early gate in
// PersistentPreRunE must not interfere with autoMigrateOnVersionBump's normal
// version-bump reconciliation.
func TestAutoMigrateStillRunsWithoutFreeze(t *testing.T) {
	bd, dir := setupMigrationFreezeWorkspace(t)

	localVersionPath := filepath.Join(dir, ".beads", localVersionFile)
	if err := os.WriteFile(localVersionPath, []byte("0.0.1\n"), 0644); err != nil {
		t.Fatalf("writing fake old %s: %v", localVersionFile, err)
	}

	stdout, stderr, code := runBDMigrationFreezeWithEnv(t, bd, dir, []string{"BD_DEBUG=1"},
		"create", "normal issue", "-p", "2", "--json")

	if code != 0 {
		t.Fatalf("bd create failed (exit %d) with no freeze marker present:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "auto-migrate:") {
		t.Errorf("expected autoMigrateOnVersionBump to run (an 'auto-migrate:' debug log line) when not frozen, got none:\nstderr:\n%s", stderr)
	}
}
