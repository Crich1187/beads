package doltretry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsRemoteCloneTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection refused"), false},
		{"chunkSource exact", errors.New("manifest referenced table file for which there is no chunkSource."), true},
		{"chunkSource mixed case", errors.New("Manifest Referenced Table File For Which There Is No ChunkSource."), true},
		{"read only manifest", errors.New("cannot update manifest: database is read only"), true},
		{"dangling chunk", errors.New("dangling chunk reference: hash abc123 referenced but not present"), true},
		{"syntax error", errors.New("Error 1064: syntax error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRemoteCloneTransientError(tt.err)
			if got != tt.want {
				t.Errorf("IsRemoteCloneTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestCloneWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	target := t.TempDir()
	called := 0
	err := CloneWithRetry(context.Background(), target, func() error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Errorf("called %d times, want 1", called)
	}
}

func TestCloneWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	target := t.TempDir()
	// Create a file inside target so we can verify cleanup ran between attempts.
	marker := filepath.Join(target, "partial")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	called := 0
	err := CloneWithRetry(context.Background(), target, func() error {
		called++
		if called < 2 {
			return errors.New("manifest referenced table file for which there is no chunkSource.")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 2 {
		t.Errorf("called %d times, want 2", called)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("expected partial target to be removed before retry")
	}
}

func TestCloneWithRetry_ReturnsNonTransientImmediately(t *testing.T) {
	target := t.TempDir()
	called := 0
	err := CloneWithRetry(context.Background(), target, func() error {
		called++
		return errors.New("authentication failed")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if called != 1 {
		t.Errorf("called %d times, want 1", called)
	}
}

func TestCloneWithRetry_RespectsContextCancellation(t *testing.T) {
	target := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	called := 0
	err := CloneWithRetry(ctx, target, func() error {
		called++
		cancel()
		return errors.New("manifest referenced table file for which there is no chunkSource.")
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if called != 1 {
		t.Errorf("called %d times, want 1", called)
	}
}

func TestCloneWithRetry_ExhaustedRetries(t *testing.T) {
	target := t.TempDir()
	t.Setenv(remoteCloneMaxRetriesEnv, "2")
	t.Setenv(remoteCloneTimeoutEnv, "1s")

	called := 0
	err := CloneWithRetry(context.Background(), target, func() error {
		called++
		return errors.New("manifest referenced table file for which there is no chunkSource.")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if called != 3 {
		t.Errorf("called %d times, want 3", called)
	}
	if !strings.Contains(err.Error(), "remote snapshot did not stabilize") {
		t.Errorf("expected stabilize message, got %v", err)
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		max     time.Duration
	}{
		{0, 250 * time.Millisecond},
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 5 * time.Second},
		{10, 5 * time.Second},
	}
	for _, c := range cases {
		got := backoff(c.attempt)
		if got != c.max {
			t.Errorf("backoff(%d) = %v, want %v", c.attempt, got, c.max)
		}
	}
}
