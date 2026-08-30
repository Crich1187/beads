package dolt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/githooksenv"
)

// The bootstrap clone does not route through prepareDoltCLITransferCommand;
// its env needs the git-trace scrub and the no-hooks override itself.
func TestBootstrapCloneCmdEnvIsGuarded(t *testing.T) {
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_CURL_VERBOSE", "1")

	cmd := bootstrapCloneCmd(context.Background(), "git+https://example.com/repo.git", filepath.Join(t.TempDir(), "beads"))
	if cmd.Env == nil {
		t.Fatal("bootstrapCloneCmd() left cmd.Env nil; the clone would inherit stderr-directed git tracing")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_TRACE=") || strings.HasPrefix(kv, "GIT_CURL_VERBOSE=") {
			t.Errorf("bootstrapCloneCmd() kept %q; stderr-directed git tracing must be scrubbed", kv)
		}
	}
	if got := githooksenv.Extract(cmd.Env); !strings.Contains(got, githooksenv.NoHooksParam) {
		t.Errorf("bootstrapCloneCmd() effective %s = %q, want the no-hooks override", githooksenv.ParametersEnv, got)
	}
}

// TestBootstrapFromRemoteWithDB_RejectsEmptyDatabase verifies that
// BootstrapFromRemoteWithDB returns an error when called with an
// empty database name. Callers should use cfg.GetDoltDatabase() which
// applies the fallback chain (env var -> config -> default). A silent
// fallback to "beads" here previously masked misconfiguration (GH#3029).
func TestBootstrapFromRemoteWithDB_RejectsEmptyDatabase(t *testing.T) {
	doltDir := t.TempDir()

	_, err := BootstrapFromRemoteWithDB(context.Background(), doltDir, "file:///dev/null", "")
	if err == nil {
		t.Fatal("expected error for empty database name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBootstrapFromRemoteWithDB_RejectsWhitespaceDatabase verifies that
// whitespace-only database names are also rejected (defense-in-depth).
func TestBootstrapFromRemoteWithDB_RejectsWhitespaceDatabase(t *testing.T) {
	doltDir := t.TempDir()

	_, err := BootstrapFromRemoteWithDB(context.Background(), doltDir, "file:///dev/null", "   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only database name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBootstrapFromRemoteWithDB_RejectsPathLikeDatabase verifies that
// database names containing path separators or traversal segments are
// rejected before a clone is attempted. Such names would let the pre-clone
// doltExists() existence check miss a database that was already cloned
// under a different-looking path, which previously allowed the
// failed-clone cleanup to RemoveAll a pre-existing Dolt repo
// (P1 data-loss finding, cross-vendor review 2026-07-07).
func TestBootstrapFromRemoteWithDB_RejectsPathLikeDatabase(t *testing.T) {
	for _, name := range []string{"foo/bar", "../other-db", "foo\\bar"} {
		doltDir := t.TempDir()
		_, err := BootstrapFromRemoteWithDB(context.Background(), doltDir, "file:///dev/null", name)
		if err == nil {
			t.Fatalf("expected error for path-like database name %q, got nil", name)
		}
		if !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("database name %q: unexpected error message: %v", name, err)
		}
	}
}

// TestBootstrapFromRemote_UsesDefaultDatabase verifies that the
// convenience wrapper BootstrapFromRemote explicitly passes the
// default database name rather than an empty string.
func TestBootstrapFromRemote_UsesDefaultDatabase(t *testing.T) {
	// Create a doltDir that already contains a database so the function
	// returns early (skips clone) without needing the dolt CLI.
	doltDir := t.TempDir()

	// BootstrapFromRemote should not error with "invalid database name"
	// because it passes configfile.DefaultDoltDatabase explicitly.
	// It will return false (skipped) because doltExists returns false for an
	// empty dir, then it will fail trying to run dolt clone — but the error
	// should be about dolt CLI, not about an invalid database name.
	_, err := BootstrapFromRemote(context.Background(), doltDir, "file:///dev/null")
	if err != nil && strings.Contains(err.Error(), "invalid database name") {
		t.Fatal("BootstrapFromRemote should pass an explicit, valid database name")
	}
	// Any other error (dolt CLI not found, clone failure) is fine — we only care
	// that the empty-database guard didn't fire.
}

// TestBootstrapFromGitRemoteWithDB_DeprecatedWrapper verifies that the
// deprecated BootstrapFromGitRemoteWithDB wrapper delegates correctly.
func TestBootstrapFromGitRemoteWithDB_DeprecatedWrapper(t *testing.T) {
	doltDir := t.TempDir()

	_, err := BootstrapFromGitRemoteWithDB(context.Background(), doltDir, "file:///dev/null", "")
	if err == nil {
		t.Fatal("expected error for empty database name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBootstrapFromRemoteWithDB_PreservesPreExistingCloneTarget verifies
// that a failed clone never runs cleanup on a clone target that already
// existed before this bootstrap attempt started. This is the defense in
// depth requested alongside database-name validation for the P1 data-loss
// finding (cross-vendor review 2026-07-07): a target directory not created
// by this attempt must be left untouched, even if `dolt clone` fails
// because the target already exists.
func TestBootstrapFromRemoteWithDB_PreservesPreExistingCloneTarget(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt CLI not available")
	}

	doltDir := t.TempDir()
	cloneTarget := filepath.Join(doltDir, "beads")
	if err := os.MkdirAll(cloneTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cloneTarget, "README.txt")
	if err := os.WriteFile(marker, []byte("pre-existing content"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := BootstrapFromRemoteWithDB(context.Background(), doltDir, "file:///dev/null", "beads")
	if err == nil {
		t.Fatal("expected error from failed clone, got nil")
	}
	if !strings.Contains(err.Error(), "already existed before this attempt") {
		t.Errorf("expected error to note the pre-existing target, got: %v", err)
	}
	if data, statErr := os.ReadFile(marker); statErr != nil || string(data) != "pre-existing content" {
		t.Fatalf("pre-existing clone target content was not preserved: data=%q err=%v", data, statErr)
	}
}

func TestDoltCloneArgs(t *testing.T) {
	t.Setenv("DOLT_REMOTE_USER", "")
	got := doltCloneArgs("https://example.com/repo", "/tmp/clone")
	want := []string{"clone", "https://example.com/repo", "/tmp/clone"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("doltCloneArgs() = %q, want %q", got, want)
	}

	t.Setenv("DOLT_REMOTE_USER", "alice")
	got = doltCloneArgs("https://example.com/repo", "/tmp/clone")
	want = []string{"clone", "--user", "alice", "https://example.com/repo", "/tmp/clone"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("doltCloneArgs() = %q, want %q", got, want)
	}
}

func TestRemoveFailedCloneTargetWithRetryRemovesPartialClone(t *testing.T) {
	oldDelays := failedCloneCleanupRetryDelays
	failedCloneCleanupRetryDelays = []time.Duration{0}
	t.Cleanup(func() { failedCloneCleanupRetryDelays = oldDelays })

	target := filepath.Join(t.TempDir(), "beads")
	if err := os.MkdirAll(filepath.Join(target, ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".dolt", "noms", "LOCK"), []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleaned, err := removeFailedCloneTargetWithRetry(target)
	if err != nil {
		t.Fatalf("removeFailedCloneTargetWithRetry() error = %v", err)
	}
	if !cleaned {
		t.Fatal("expected cleanup to report removing a partial Dolt clone")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected failed clone target to be removed, stat err=%v", err)
	}
}

func TestRemoveFailedCloneTargetWithRetryPreservesNonDoltTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "beads")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "README.txt"), []byte("not a dolt clone"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleaned, err := removeFailedCloneTargetWithRetry(target)
	if err != nil {
		t.Fatalf("removeFailedCloneTargetWithRetry() error = %v", err)
	}
	if cleaned {
		t.Fatal("non-Dolt target should not report cleanup")
	}
	if _, err := os.Stat(filepath.Join(target, "README.txt")); err != nil {
		t.Fatalf("non-Dolt target should be preserved, stat err=%v", err)
	}
}

func TestRemoveFailedCloneTargetWithRetryPreservesDotDoltFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "beads")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".dolt"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleaned, err := removeFailedCloneTargetWithRetry(target)
	if err != nil {
		t.Fatalf("removeFailedCloneTargetWithRetry() error = %v", err)
	}
	if cleaned {
		t.Fatal("target with non-directory .dolt marker should not report cleanup")
	}
	if _, err := os.Stat(filepath.Join(target, ".dolt")); err != nil {
		t.Fatalf("non-directory .dolt marker should be preserved, stat err=%v", err)
	}
}

func TestFormatFailedCloneTargetErrorCleanupSucceeded(t *testing.T) {
	err := formatFailedCloneTargetError(errors.New("exit status 1"), []byte("clone failed"), `C:\tmp\beads`, true, nil)

	msg := err.Error()
	if !strings.Contains(msg, "Cleaned up failed clone target") {
		t.Fatalf("expected cleanup success note, got:\n%s", msg)
	}
	if !strings.Contains(msg, "retry `bd bootstrap`") {
		t.Fatalf("expected retry guidance, got:\n%s", msg)
	}
}

func TestFormatFailedCloneTargetErrorNoCleanup(t *testing.T) {
	err := formatFailedCloneTargetError(errors.New("exit status 1"), []byte("clone failed before target"), `C:\tmp\beads`, false, nil)

	msg := err.Error()
	if strings.Contains(msg, "Cleaned up failed clone target") {
		t.Fatalf("should not claim cleanup when no cleanup ran, got:\n%s", msg)
	}
	if !strings.Contains(msg, "clone failed before target") {
		t.Fatalf("expected original output, got:\n%s", msg)
	}
}

func TestFormatFailedCloneTargetErrorCleanupFailed(t *testing.T) {
	err := formatFailedCloneTargetError(errors.New("exit status 1"), []byte("unable to clean up failed clone"), `C:\tmp\beads`, true, errors.New("file is in use"))

	msg := err.Error()
	for _, want := range []string{"Could not clean up failed clone target", "Windows", ".dolt/noms/LOCK", "Stop stuck dolt/bd processes"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected %q in error, got:\n%s", want, msg)
		}
	}
}

// TestBootstrapFromRemoteWithDB_RetriesTransientCloneError verifies that
// BootstrapFromRemoteWithDB retries `dolt clone` when another host's push
// leaves the remote snapshot temporarily inconsistent. A fake dolt binary
// fails the first two attempts with the Dolt "no chunkSource" error, then
// succeeds and creates the expected database directory.
func TestBootstrapFromRemoteWithDB_RetriesTransientCloneError(t *testing.T) {
	fakeDir, stateDir := installFakeDoltForBootstrap(t, 2)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("BEADS_FAKE_DOLT_STATE", stateDir)

	doltDir := t.TempDir()
	synced, err := BootstrapFromRemoteWithDB(context.Background(), doltDir, "file:///tmp/test-remote", "beads")
	if err != nil {
		t.Fatalf("BootstrapFromRemoteWithDB should retry and succeed: %v", err)
	}
	if !synced {
		t.Fatal("expected synced=true")
	}

	if !doltExists(doltDir) {
		t.Errorf("expected dolt database to exist in %s after retry", doltDir)
	}

	counter := fakeDoltInvocationCountBootstrap(stateDir)
	if counter != 3 {
		t.Errorf("expected 3 dolt clone invocations (2 failures + 1 success), got %d", counter)
	}
}

// installFakeDoltForBootstrap creates an executable `dolt` script in a temp
// directory and a separate state directory. The script fails the first
// failCount calls with the "no chunkSource" error, then succeeds.
func installFakeDoltForBootstrap(t *testing.T, failCount int) (string, string) {
	t.Helper()

	fakeDir := t.TempDir()
	stateDir := filepath.Join(fakeDir, "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/bash
set -e
state="${BEADS_FAKE_DOLT_STATE}"`
	script += `
counter_file="$state/counter"
n=$(cat "$counter_file" 2>/dev/null || echo 0)
echo $((n+1)) > "$counter_file"
if [ "$n" -lt "` + strconv.Itoa(failCount) + `" ]; then
  echo "Error: manifest referenced table file for which there is no chunkSource." >&2
  exit 1
fi
target="${@: -1}"
mkdir -p "$target/.dolt"
echo '{}' > "$target/.dolt/repo_state.json"
echo '{}' > "$target/.dolt/config.json"
`
	fakeDoltPath := filepath.Join(fakeDir, "dolt")
	if err := os.WriteFile(fakeDoltPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return fakeDir, stateDir
}

func fakeDoltInvocationCountBootstrap(stateDir string) int {
	data, err := os.ReadFile(filepath.Join(stateDir, "counter"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}
