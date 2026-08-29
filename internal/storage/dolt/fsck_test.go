package dolt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrePushFSCK_EmptyCLIDir verifies that prePushFSCK is a no-op when
// CLIDir is empty (no local noms store configured).
func TestPrePushFSCK_EmptyCLIDir(t *testing.T) {
	t.Parallel()
	s := &DoltStore{dbPath: "", database: "test"}
	if err := s.prePushFSCK(context.Background()); err != nil {
		t.Fatalf("expected nil for empty CLIDir, got %v", err)
	}
}

// TestPrePushFSCK_NoNomsDir verifies that prePushFSCK is a no-op when
// CLIDir exists but .dolt/noms does not (uninitialized or non-dolt directory).
func TestPrePushFSCK_NoNomsDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := &DoltStore{dbPath: tmp, database: "mydb"}
	// CLIDir() = tmp/mydb, which doesn't exist and has no .dolt/noms
	if err := s.prePushFSCK(context.Background()); err != nil {
		t.Fatalf("expected nil when .dolt/noms absent, got %v", err)
	}
}

// TestPrePushFSCK_CleanDB verifies that prePushFSCK passes on a fresh
// dolt-initialized database with no corruption.
func TestPrePushFSCK_CleanDB(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not in PATH")
	}

	tmp := t.TempDir()
	dbDir := filepath.Join(tmp, "mydb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	initCmd := exec.Command("dolt", "init", "--name", "test", "--email", "test@example.com")
	initCmd.Dir = dbDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init: %v\n%s", err, out)
	}

	s := &DoltStore{dbPath: tmp, database: "mydb"}
	if err := s.prePushFSCK(context.Background()); err != nil {
		t.Fatalf("expected nil on clean DB, got %v", err)
	}
}

// TestPrePushFSCK_UnopenableDB verifies that prePushFSCK logs a warning and
// proceeds (returns nil) when dolt fsck cannot open the database. This avoids
// misleading users with a corruption warning for environmental / tooling
// failures. Example: dolthub/dolt#10915 (Windows url.Parse bug pre-v1.86.4)
// caused fsck to fail-to-open healthy databases, which the previous wrapper
// reported as "dangling chunk reference: aborting push to prevent propagating
// corrupt chunks".
//
// We simulate the unopenable state by creating a .dolt/noms directory without
// running dolt init — fsck prints "Could not open dolt database" and exits
// non-zero.
func TestPrePushFSCK_UnopenableDB(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not in PATH")
	}

	tmp := t.TempDir()
	dbDir := filepath.Join(tmp, "mydb")
	// Create .dolt/noms so the skip check passes, but don't init the repo.
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt", "noms"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := &DoltStore{dbPath: tmp, database: "mydb"}
	if err := s.prePushFSCK(context.Background()); err != nil {
		t.Fatalf("expected nil when fsck cannot open db (should warn and proceed), got %v", err)
	}
}

// TestFsckCouldNotOpen verifies the helper identifies both known dolt
// "couldn't open" phrasings and does not classify actual integrity failures
// (or unrelated output) as open-failures.
func TestFsckCouldNotOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "windows url.Parse bug pre-1.86.4 (dolthub/dolt#10915)",
			output: `Could not open dolt database: CreateFile \C:\Users\x\.beads\...\.dolt\noms: The filename, directory name, or volume label syntax is incorrect.`,
			want:   true,
		},
		{
			name:   "uninitialized .dolt directory",
			output: "The current directories repository state is invalid\nopen .dolt/repo_state.json: no such file or directory",
			want:   true,
		},
		{
			name:   "actual dangling chunk reference (must still abort)",
			output: "dangling chunk reference: hash abc123 referenced but not present",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fsckCouldNotOpen(tc.output); got != tc.want {
				t.Errorf("fsckCouldNotOpen(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestClassifyFSCKFailure verifies that classifyFSCKFailure maps every failure
// mode to the correct outcome, and includes negative sentinel checks to guard
// against misclassification.
func TestClassifyFSCKFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		parentErr    error
		fsckErr      error
		output       string
		wantNil      bool
		wantIs       error
		wantIsNot    []error
		wantContains string
		wantAbsent   string
	}{
		{
			name:      "non-empty output + DeadlineExceeded -> ErrDanglingReference, not ErrFSCKTimeout",
			parentErr: context.DeadlineExceeded,
			fsckErr:   context.DeadlineExceeded,
			output:    "dangling chunk reference: hash abc123 not found",
			wantIs:    ErrDanglingReference,
			wantIsNot: []error{ErrFSCKTimeout},
		},
		{
			name:    "non-empty could-not-open output -> nil",
			output:  "Could not open dolt database: some reason",
			wantNil: true,
		},
		{
			name:         "parent canceled -> cancellation error, not dangling or timeout",
			parentErr:    context.Canceled,
			fsckErr:      context.Canceled,
			wantIs:       context.Canceled,
			wantIsNot:    []error{ErrDanglingReference, ErrFSCKTimeout},
			wantContains: "interrupted",
		},
		{
			name:         "parent DeadlineExceeded -> ErrFSCKTimeout with caller-timeout guidance",
			parentErr:    context.DeadlineExceeded,
			fsckErr:      context.DeadlineExceeded,
			wantIs:       ErrFSCKTimeout,
			wantIsNot:    []error{ErrDanglingReference},
			wantContains: "dolt.auto-push-timeout",
			wantAbsent:   "BEADS_FSCK_TIMEOUT environment variable",
		},
		{
			name:         "own fsck DeadlineExceeded -> ErrFSCKTimeout with BEADS_FSCK_TIMEOUT guidance",
			fsckErr:      context.DeadlineExceeded,
			wantIs:       ErrFSCKTimeout,
			wantIsNot:    []error{ErrDanglingReference},
			wantContains: "BEADS_FSCK_TIMEOUT",
		},
		{
			name:      "generic failure with empty output -> ErrDanglingReference",
			wantIs:    ErrDanglingReference,
			wantIsNot: []error{ErrFSCKTimeout},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyFSCKFailure(tc.parentErr, tc.fsckErr, tc.output)

			if tc.wantNil {
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want non-nil error, got nil")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantIs, err)
			}
			for _, notErr := range tc.wantIsNot {
				if errors.Is(err, notErr) {
					t.Errorf("errors.Is(err, %v) = true, want false; err = %v", notErr, err)
				}
			}
			if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error message %q does not contain %q", err.Error(), tc.wantContains)
			}
			if tc.wantAbsent != "" && strings.Contains(err.Error(), tc.wantAbsent) {
				t.Errorf("error message %q must not contain %q", err.Error(), tc.wantAbsent)
			}
		})
	}
}

func TestClassifyFSCKFailure_CallerVsOwnTimeout(t *testing.T) {
	t.Parallel()
	callerErr := classifyFSCKFailure(context.DeadlineExceeded, context.DeadlineExceeded, "")
	ownErr := classifyFSCKFailure(nil, context.DeadlineExceeded, "")

	if callerErr == nil || ownErr == nil {
		t.Fatal("both cases must return non-nil errors")
	}
	if !strings.Contains(callerErr.Error(), "dolt.auto-push-timeout") {
		t.Errorf("caller-deadline message should name dolt.auto-push-timeout; got: %q", callerErr)
	}
	if strings.Contains(callerErr.Error(), "BEADS_FSCK_TIMEOUT environment variable") {
		t.Errorf("caller-deadline message must not say BEADS_FSCK_TIMEOUT environment variable; got: %q", callerErr)
	}
	if !strings.Contains(ownErr.Error(), "BEADS_FSCK_TIMEOUT") {
		t.Errorf("own-timeout message should name BEADS_FSCK_TIMEOUT; got: %q", ownErr)
	}
	if strings.Contains(ownErr.Error(), "dolt.auto-push-timeout") {
		t.Errorf("own-timeout message must not name dolt.auto-push-timeout; got: %q", ownErr)
	}
}

func TestFsckTimeoutDuration(t *testing.T) {
	t.Run("valid duration honored", func(t *testing.T) {
		t.Setenv(fsckTimeoutEnv, "2m")
		if got := fsckTimeoutDuration(); got != 2*time.Minute {
			t.Errorf("want 2m, got %v", got)
		}
	})
	t.Run("bare seconds honored", func(t *testing.T) {
		t.Setenv(fsckTimeoutEnv, "90")
		if got := fsckTimeoutDuration(); got != 90*time.Second {
			t.Errorf("want 90s, got %v", got)
		}
	})
	t.Run("unset returns default", func(t *testing.T) {
		t.Setenv(fsckTimeoutEnv, "")
		if got := fsckTimeoutDuration(); got != fsckTimeout {
			t.Errorf("want %v, got %v", fsckTimeout, got)
		}
	})
	t.Run("invalid returns default", func(t *testing.T) {
		t.Setenv(fsckTimeoutEnv, "not-a-duration")
		if got := fsckTimeoutDuration(); got != fsckTimeout {
			t.Errorf("want %v, got %v", fsckTimeout, got)
		}
	})
}
