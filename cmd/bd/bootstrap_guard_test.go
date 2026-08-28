//go:build cgo

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
)

// TestIsGitCodeRepoURL verifies the URL classifier that guards sync.remote
// against accidental git code-repository URLs.
func TestIsGitCodeRepoURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		// .git suffix — always a code-repo signal regardless of host
		{"git+ssh://github.com/org/repo.git", true},
		{"https://github.com/org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"https://gitlab.com/org/repo.git", true},
		{"git+https://bitbucket.org/org/repo.git", true},
		{"https://codeberg.org/org/repo.git", true},
		// .git on unknown host: still blocked (Dolt DBs never use .git suffix)
		{"git+ssh://my-dolt.example.com/org/repo.git", true},

		// Well-known forges without .git suffix
		{"https://github.com/org/repo", true},
		{"git+ssh://github.com/org/db", true},
		{"https://gitlab.com/org/group/repo", true},
		{"https://bitbucket.org/org/repo", true},
		{"https://codeberg.org/org/repo", true},

		// github.com / gitlab.com subdomains
		{"https://raw.github.com/org/repo", true},
		{"https://api.github.com/repos/org/repo", true},
		{"https://example.gitlab.com/org/repo", true},

		// Valid Dolt-native schemes — never blocked
		{"dolthub://myorg/mydb", false},
		{"s3://bucket/path", false},
		{"gs://bucket/path", false},
		{"az://container/path", false},
		{"file:///tmp/doltdb", false},

		// git+ssh to a self-hosted Dolt remote — NOT a code repo
		{"git+ssh://my-self-hosted-dolt.example.com/org/db", false},
		{"https://doltremoteapi.dolthub.com/org/db", false},
		{"https://doltremoteapi.example.com/mydb", false},

		// SCP-style to non-forge — allowed
		{"git@my-dolt.example.com:org/db", false},

		// Edge: empty string
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := isGitCodeRepoURL(tt.url)
			if got != tt.want {
				t.Errorf("isGitCodeRepoURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

// setupSyncRemoteConfig writes sync.remote to .beads/config.yaml and
// initializes the config subsystem so resolveSyncRemote() can read it.
// Returns a cleanup function that resets config state.
func setupSyncRemoteConfig(t *testing.T, beadsDir, remote string) func() {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(beadsDir, "config.yaml"),
		[]byte("sync.remote: "+remote+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	config.ResetForTesting()
	t.Setenv("BEADS_DIR", beadsDir)
	t.Setenv("BEADS_TEST_IGNORE_REPO_CONFIG", "1")
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	return func() { config.ResetForTesting() }
}

// stubProbeGitRemoteDoltData replaces the refs/dolt/data probe for the
// duration of a test. Every test in this file must install a stub: the real
// probe shells out to `git ls-remote` against whatever host the fixture URL
// names.
func stubProbeGitRemoteDoltData(t *testing.T, fn func(string) (bool, error)) {
	t.Helper()
	orig := probeGitRemoteDoltData
	probeGitRemoteDoltData = fn
	t.Cleanup(func() { probeGitRemoteDoltData = orig })
}

// forgeSyncRemoteForms enumerates the shapes a git-forge sync.remote arrives
// in, with the git+ URL normalizeRemoteURL produces for each. Every one of
// them is a value bd itself writes (bd init from a git origin, bd dolt remote
// add, a hand-edited config.yaml, or finalizeSyncedBootstrap).
var forgeSyncRemoteForms = []struct {
	name      string
	remote    string
	wantClone string
}{
	{"raw https with .git", "https://github.com/org/repo.git", "git+https://github.com/org/repo.git"},
	{"raw https without .git", "https://github.com/org/repo", "git+https://github.com/org/repo"},
	{"scp style", "git@github.com:org/repo.git", "git+ssh://git@github.com/org/repo.git"},
	{"ssh scheme", "ssh://git@github.com/org/repo.git", "git+ssh://git@github.com/org/repo.git"},
	{"already git+https", "git+https://github.com/org/repo.git", "git+https://github.com/org/repo.git"},
	{"git+ssh without user", "git+ssh://github.com/org/repo.git", "git+ssh://github.com/org/repo.git"},
	{"gitlab", "https://gitlab.com/group/repo.git", "git+https://gitlab.com/group/repo.git"},
}

// newForgeSyncRemoteWorkspace builds a workspace whose config.yaml carries
// remote as sync.remote and chdirs into it, returning the .beads path.
func newForgeSyncRemoteWorkspace(t *testing.T, remote string) string {
	t.Helper()
	snapshotBootstrapEnv(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cleanup := setupSyncRemoteConfig(t, beadsDir, remote)
	t.Cleanup(cleanup)

	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	return beadsDir
}

// TestDetectBootstrapAction_GitForgeSyncRemote_ProbeRouting covers the #5743 /
// #5663 fix: a git-forge sync.remote is a supported Dolt remote, so bootstrap
// probes refs/dolt/data and routes on the answer instead of rejecting the URL
// outright (which printed a success tick and exited 0 with no database).
func TestDetectBootstrapAction_GitForgeSyncRemote_ProbeRouting(t *testing.T) {
	t.Run("data present clones the git+ form", func(t *testing.T) {
		for _, tc := range forgeSyncRemoteForms {
			t.Run(tc.name, func(t *testing.T) {
				beadsDir := newForgeSyncRemoteWorkspace(t, tc.remote)

				var probed []string
				stubProbeGitRemoteDoltData(t, func(url string) (bool, error) {
					probed = append(probed, url)
					return true, nil
				})

				plan := detectBootstrapAction(beadsDir, configfile.DefaultConfig())

				if plan.Action != "sync" {
					t.Fatalf("action=%q, want %q (remote carries refs/dolt/data)", plan.Action, "sync")
				}
				if plan.SyncRemote != tc.wantClone {
					t.Errorf("SyncRemote=%q, want %q", plan.SyncRemote, tc.wantClone)
				}
				// The durable #4421 pin: no raw forge http(s) URL may ever
				// reach DOLT_CLONE, because Dolt routes those through the
				// remotesapi client and retries forever at ~1000% CPU. The
				// git+ prefix is what selects the git remote factory.
				if !strings.HasPrefix(plan.SyncRemote, "git+") {
					t.Errorf("SyncRemote=%q lacks the git+ prefix; a raw forge URL must never reach DOLT_CLONE (#4421)", plan.SyncRemote)
				}
				if plan.Blocked {
					t.Error("Blocked=true on a successful sync plan")
				}
				if len(probed) != 1 || probed[0] != tc.wantClone {
					t.Errorf("probe calls = %v, want exactly [%q]", probed, tc.wantClone)
				}
			})
		}
	})

	t.Run("no data falls through to init", func(t *testing.T) {
		beadsDir := newForgeSyncRemoteWorkspace(t, "https://github.com/org/repo.git")
		stubProbeGitRemoteDoltData(t, func(string) (bool, error) { return false, nil })

		plan := detectBootstrapAction(beadsDir, configfile.DefaultConfig())

		if plan.Action != "init" {
			t.Fatalf("action=%q, want %q (remote wired before the first bd dolt push)", plan.Action, "init")
		}
		if plan.Blocked {
			t.Error("Blocked=true; a clean 'no data yet' probe is not a failure")
		}
		if plan.SyncRemote != "" {
			t.Errorf("SyncRemote=%q, want empty (nothing to clone)", plan.SyncRemote)
		}
	})

	t.Run("no data falls through to backup restore", func(t *testing.T) {
		beadsDir := newForgeSyncRemoteWorkspace(t, "https://github.com/org/repo.git")
		backupDir := filepath.Join(beadsDir, "backup")
		if err := os.MkdirAll(backupDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(backupDir, "issues.jsonl"), []byte(`{"id":"bd-1"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stubProbeGitRemoteDoltData(t, func(string) (bool, error) { return false, nil })

		plan := detectBootstrapAction(beadsDir, configfile.DefaultConfig())

		// Proves the fall-through runs the whole remaining detector chain,
		// not just the fresh-init tail.
		if plan.Action != "restore" {
			t.Fatalf("action=%q, want %q (backup must still be found after the probe falls through)", plan.Action, "restore")
		}
		if plan.BackupDir != backupDir {
			t.Errorf("BackupDir=%q, want %q", plan.BackupDir, backupDir)
		}
	})

	t.Run("probe error blocks and never clones", func(t *testing.T) {
		for _, tc := range forgeSyncRemoteForms {
			t.Run(tc.name, func(t *testing.T) {
				beadsDir := newForgeSyncRemoteWorkspace(t, tc.remote)
				probeErr := errors.New("exit status 128: Permission denied (publickey)")
				stubProbeGitRemoteDoltData(t, func(string) (bool, error) { return false, probeErr })

				plan := detectBootstrapAction(beadsDir, configfile.DefaultConfig())

				if plan.Action != "none" {
					t.Fatalf("action=%q, want %q (UNKNOWN must fail closed)", plan.Action, "none")
				}
				if !plan.Blocked {
					t.Error("Blocked=false; an unverifiable remote must exit non-zero, not print a success tick")
				}
				if plan.SyncRemote != "" {
					t.Errorf("SyncRemote=%q, want empty (an unverified URL must not propagate to the clone)", plan.SyncRemote)
				}
				if plan.BlockedRemote != tc.wantClone {
					t.Errorf("BlockedRemote=%q, want %q (needed for the ls-remote hint)", plan.BlockedRemote, tc.wantClone)
				}
				if !strings.Contains(plan.Reason, tc.remote) {
					t.Errorf("Reason=%q does not name the offending URL %q", plan.Reason, tc.remote)
				}
				if !strings.Contains(plan.Reason, "publickey") {
					t.Errorf("Reason=%q does not carry the probe error", plan.Reason)
				}
			})
		}
	})

	t.Run("routing is cwd independent in a monorepo subdir", func(t *testing.T) {
		remote := "https://github.com/org/repo.git"
		beadsDir := newForgeSyncRemoteWorkspace(t, remote)

		subDir := filepath.Join(filepath.Dir(beadsDir), "sub", "dir")
		if err := os.MkdirAll(subDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(subDir); err != nil {
			t.Fatal(err)
		}

		var probed []string
		stubProbeGitRemoteDoltData(t, func(url string) (bool, error) {
			probed = append(probed, url)
			return true, nil
		})

		plan := detectBootstrapAction(beadsDir, configfile.DefaultConfig())

		if plan.Action != "sync" {
			t.Fatalf("action=%q, want %q from a subdirectory", plan.Action, "sync")
		}
		if len(probed) != 1 || probed[0] != "git+https://github.com/org/repo.git" {
			t.Errorf("probe calls = %v, want exactly [%q]", probed, "git+https://github.com/org/repo.git")
		}
	})
}

// TestDetectBootstrapAction_ValidDoltSyncRemoteUnchanged verifies that a valid
// Dolt remote URL is still accepted as-is (no regression from Layer 1 guard).
func TestDetectBootstrapAction_ValidDoltSyncRemoteUnchanged(t *testing.T) {
	for _, remote := range []string{
		"https://doltremoteapi.dolthub.com/org/db",
		"dolthub://myorg/mydb",
		"git+ssh://my-self-hosted-dolt.example.com/org/db",
	} {
		t.Run(remote, func(t *testing.T) {
			snapshotBootstrapEnv(t)

			tmpDir := t.TempDir()
			beadsDir := filepath.Join(tmpDir, ".beads")
			if err := os.MkdirAll(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			cleanup := setupSyncRemoteConfig(t, beadsDir, remote)
			defer cleanup()

			oldWd, _ := os.Getwd()
			defer func() { _ = os.Chdir(oldWd) }()
			if err := os.Chdir(tmpDir); err != nil {
				t.Fatal(err)
			}

			// Non-forge remotes take the trusted-as-is path and must never
			// pay for an ls-remote — a Dolt remotesapi endpoint has no
			// refs/dolt/data to probe in the first place.
			stubProbeGitRemoteDoltData(t, func(url string) (bool, error) {
				t.Fatalf("probed non-forge remote %q; the trusted-as-is path must not probe", url)
				return false, nil
			})

			cfg := configfile.DefaultConfig()
			plan := detectBootstrapAction(beadsDir, cfg)

			if plan.Action != "sync" {
				t.Errorf("remote=%q: action=%q, want %q (valid Dolt remote must not be blocked)", remote, plan.Action, "sync")
			}
			if plan.SyncRemote != remote {
				t.Errorf("remote=%q: SyncRemote=%q, want same URL", remote, plan.SyncRemote)
			}
		})
	}
}

// TestDetectBootstrapAction_ServerTransientRestart_DBFoundAfterRetry verifies
// that when the server is reachable but the DB appears absent on the first probe
// (e.g. during a managed Dolt restart), the retry loop waits and eventually finds
// the DB — returning action=none instead of falling through to sync/init
// .
func TestDetectBootstrapAction_ServerTransientRestart_DBFoundAfterRetry(t *testing.T) {
	t.Setenv("BEADS_DOLT_DATA_DIR", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(tmpDir, "dolt-data")
	if err := os.MkdirAll(filepath.Join(doltDataDir, "mydb"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DOLT_DATA_DIR", doltDataDir)

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	cfg := configfile.DefaultConfig()
	cfg.DoltMode = configfile.DoltModeServer
	cfg.DoltDatabase = "mydb"
	cfg.DoltDataDir = doltDataDir

	// Simulate a restart: DB absent for first 2 probes, then found.
	callCount := 0
	origCheck := checkBootstrapServerDB
	checkBootstrapServerDB = func(_ bootstrapServerProbeConfig) bootstrapServerDBCheck {
		callCount++
		if callCount <= 2 {
			return bootstrapServerDBCheck{Exists: false, Reachable: true}
		}
		return bootstrapServerDBCheck{Exists: true, Reachable: true}
	}
	defer func() { checkBootstrapServerDB = origCheck }()

	origDelay := bootstrapRetryDelay
	bootstrapRetryDelay = func(time.Duration) {}
	defer func() { bootstrapRetryDelay = origDelay }()

	plan := detectBootstrapAction(beadsDir, cfg)

	if plan.Action != "none" {
		t.Errorf("action=%q, want %q — DB found after retry must suppress clone", plan.Action, "none")
	}
	if !plan.HasExisting {
		t.Error("HasExisting=false, want true — DB was found on the retry probe")
	}
	if callCount < 3 {
		t.Errorf("checkBootstrapServerDB called %d times, want >=3 (retry must fire)", callCount)
	}
}

// TestDetectBootstrapAction_ServerGenuinelyAbsent_FallsThrough verifies that
// when the server is reachable but the DB is genuinely absent after all retries,
// detection falls through to init (no change from pre-retry behavior).
func TestDetectBootstrapAction_ServerGenuinelyAbsent_FallsThrough(t *testing.T) {
	t.Setenv("BEADS_DOLT_DATA_DIR", "")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(tmpDir, "dolt-data")
	if err := os.MkdirAll(filepath.Join(doltDataDir, "mydb"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DOLT_DATA_DIR", doltDataDir)

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	cfg := configfile.DefaultConfig()
	cfg.DoltMode = configfile.DoltModeServer
	cfg.DoltDatabase = "mydb"
	cfg.DoltDataDir = doltDataDir

	// DB is always absent — simulates a genuinely missing database.
	origCheck := checkBootstrapServerDB
	checkBootstrapServerDB = func(_ bootstrapServerProbeConfig) bootstrapServerDBCheck {
		return bootstrapServerDBCheck{Exists: false, Reachable: true}
	}
	defer func() { checkBootstrapServerDB = origCheck }()

	origDelay := bootstrapRetryDelay
	bootstrapRetryDelay = func(time.Duration) {}
	defer func() { bootstrapRetryDelay = origDelay }()

	plan := detectBootstrapAction(beadsDir, cfg)

	if plan.Action == "none" {
		t.Errorf("action=none, want non-none — genuinely absent DB must not block recovery")
	}
	// No backup or JSONL → should fall through to init.
	if plan.Action != "init" {
		t.Errorf("action=%q, want %q — no backup/jsonl available, fresh init expected", plan.Action, "init")
	}
}
