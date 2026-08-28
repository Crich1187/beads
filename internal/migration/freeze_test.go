package migration

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeMarker writes a freeze marker at dir/MIGRATION-FREEZE and returns its path.
func writeMarker(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing marker at %s: %v", path, err)
	}
	return path
}

func TestFindNoMarker(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// A real marker could exist above the temp root on a developer machine;
	// the env override pins detection to a path that certainly does not exist.
	t.Setenv(EnvFreezeFile, filepath.Join(dir, "definitely-not-here"))

	if got := Find(); got != "" {
		t.Fatalf("Find() = %q, want \"\" (no marker present)", got)
	}
}

func TestFindMarkerInCwd(t *testing.T) {
	dir := t.TempDir()
	want := writeMarker(t, dir, "operator\t2026-08-16T12:00:00Z\tmigrating\n")
	t.Chdir(dir)

	got := Find()
	if got == "" {
		t.Fatalf("Find() = \"\", want the marker in the working directory")
	}
	if !sameFile(t, got, want) {
		t.Errorf("Find() = %q, want %q", got, want)
	}
}

func TestFindWalksUpToGrandparent(t *testing.T) {
	root := t.TempDir()
	want := writeMarker(t, root, "")
	deep := filepath.Join(root, "rig", "repo")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatalf("creating %s: %v", deep, err)
	}
	t.Chdir(deep)

	got := Find()
	if got == "" {
		t.Fatalf("Find() = \"\", want the marker two levels up at %q", want)
	}
	if !sameFile(t, got, want) {
		t.Errorf("Find() = %q, want %q", got, want)
	}
}

// TestFindEnvOverrideAuthoritative pins both directions of the override: an
// existing file at EnvFreezeFile freezes even with no ancestor marker, and a
// missing file at EnvFreezeFile does NOT freeze even when an ancestor marker
// exists. The second half is what makes the variable a usable opt-out.
func TestFindEnvOverrideAuthoritative(t *testing.T) {
	t.Run("existing file freezes without any ancestor marker", func(t *testing.T) {
		work := t.TempDir()
		elsewhere := t.TempDir()
		override := filepath.Join(elsewhere, "freeze-marker")
		if err := os.WriteFile(override, []byte("operator\t\treason\n"), 0644); err != nil {
			t.Fatalf("writing override marker: %v", err)
		}
		t.Chdir(work)
		t.Setenv(EnvFreezeFile, override)

		if got := Find(); got != override {
			t.Errorf("Find() = %q, want the override path %q", got, override)
		}
	})

	t.Run("missing file wins over an ancestor marker", func(t *testing.T) {
		root := t.TempDir()
		writeMarker(t, root, "operator\t\treason\n")
		deep := filepath.Join(root, "nested")
		if err := os.MkdirAll(deep, 0755); err != nil {
			t.Fatalf("creating %s: %v", deep, err)
		}
		t.Chdir(deep)
		t.Setenv(EnvFreezeFile, filepath.Join(root, "no-such-marker"))

		if got := Find(); got != "" {
			t.Errorf("Find() = %q, want \"\" — the override must skip the ancestor walk entirely", got)
		}
	})
}

func TestReadFileParsesOperatorTimestampReason(t *testing.T) {
	dir := t.TempDir()
	ts := "2026-08-16T12:00:00Z"
	path := writeMarker(t, dir, "operator\t"+ts+"\tdolt v2 migration\n")

	info := ReadFile(path)
	if info == nil {
		t.Fatalf("ReadFile(%q) = nil, want parsed Info", path)
	}
	if info.Operator != "operator" {
		t.Errorf("Operator = %q, want %q", info.Operator, "operator")
	}
	if info.Reason != "dolt v2 migration" {
		t.Errorf("Reason = %q, want %q", info.Reason, "dolt v2 migration")
	}
	wantTS, _ := time.Parse(time.RFC3339, ts)
	if !info.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", info.Timestamp, wantTS)
	}
}

func TestReadFileMissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	if info := ReadFile(filepath.Join(dir, FileName)); info != nil {
		t.Fatalf("ReadFile = %+v, want nil (no marker present)", info)
	}
	if info := ReadFile(""); info != nil {
		t.Fatalf("ReadFile(\"\") = %+v, want nil", info)
	}
}

func TestReadFileEmptyContentDoesNotFabricateTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := writeMarker(t, dir, "")

	info := ReadFile(path)
	if info == nil {
		t.Fatalf("ReadFile(%q) = nil for an empty-but-present marker, want non-nil Info", path)
	}
	if !info.Timestamp.IsZero() {
		t.Errorf("Timestamp = %v, want zero value (no recorded timestamp to report — must not fabricate one)", info.Timestamp)
	}
	if info.Operator != "" || info.Reason != "" {
		t.Errorf("Operator/Reason = %q/%q, want both empty for an empty marker", info.Operator, info.Reason)
	}
}

func TestReadFileMalformedContentDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := writeMarker(t, dir, "not tab separated at all")

	info := ReadFile(path)
	if info == nil {
		t.Fatalf("ReadFile(%q) = nil for a malformed-but-present marker, want non-nil Info with best-effort fields", path)
	}
	if info.Operator != "not tab separated at all" {
		t.Errorf("Operator = %q, want the whole line back (no tabs to split on)", info.Operator)
	}
}

// sameFile compares two paths by identity rather than string equality: on
// macOS t.TempDir() lives under /var, a symlink to /private/var, so os.Getwd
// in Find returns the resolved spelling while the test holds the original.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(ai, bi)
}
