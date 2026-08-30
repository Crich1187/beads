package doltretry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// remoteCloneMaxRetriesEnv sets the maximum number of retry attempts for a
	// remote clone or pull that fails with a transient consistency error.
	remoteCloneMaxRetriesEnv = "BEADS_REMOTE_CLONE_MAX_RETRIES"

	// remoteCloneTimeoutEnv bounds the total time spent waiting for a remote
	// snapshot to stabilize during clone/pull retry.
	remoteCloneTimeoutEnv = "BEADS_REMOTE_CLONE_TIMEOUT"

	defaultRemoteCloneMaxRetries = 10
	defaultRemoteCloneTimeout    = 2 * time.Minute
)

// IsRemoteCloneTransientError reports whether an error from `dolt clone` or
// `dolt pull` is likely transient due to a concurrent remote push. Remote
// writes are atomic at the ref level, but readers may observe a manifest that
// references table-file objects that have not yet propagated to the read path
// (replication lag, in-flight push, or partial fetch). Retrying gives the
// remote a chance to reach a consistent snapshot.
func IsRemoteCloneTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		// Dolt NBS reports this when the manifest references a table file that
		// is not available locally yet. During a concurrent push the new table
		// files may land slightly after the manifest becomes visible.
		"manifest referenced table file for which there is no chunksource",
		// Dolt occasionally returns this transient error while another writer
		// holds the manifest lock on a remote-backed store.
		"cannot update manifest: database is read only",
		// A push-in-progress can leave a manifest whose chunks are not yet
		// reachable from the read side. Retry rather than fail the clone.
		"dangling chunk reference",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// CloneWithRetry runs cloneFn repeatedly until it succeeds, a non-transient
// error occurs, or the retry budget is exhausted. The target directory is
// removed before each retry so no partial clone is reused.
func CloneWithRetry(ctx context.Context, target string, cloneFn func() error) error {
	cleanup := func() {
		// Best-effort removal of a partial clone. If the directory cannot be
		// removed we still attempt the next clone; dolt clone will either
		// overwrite or fail with its own error.
		_ = os.RemoveAll(target)
	}
	return withRemoteReadRetry(ctx, cleanup, cloneFn)
}

// PullWithRetry runs pullFn repeatedly until it succeeds, a non-transient
// error occurs, or the retry budget is exhausted. Unlike CloneWithRetry, no
// cleanup is performed between attempts because the caller already has a
// valid cached clone that should not be discarded.
func PullWithRetry(ctx context.Context, pullFn func() error) error {
	return withRemoteReadRetry(ctx, func() {}, pullFn)
}

// withRemoteReadRetry implements bounded retry with exponential backoff for
// transient remote-read errors.
func withRemoteReadRetry(ctx context.Context, cleanup func(), op func() error) error {
	maxRetries, timeout := remoteCloneRetryConfig()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			cleanup()
		}

		err := op()
		if err == nil {
			return nil
		}
		lastErr = err

		if !IsRemoteCloneTransientError(err) {
			return err
		}

		wait := minDuration(backoff(attempt), time.Until(deadline))
		if wait <= 0 {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	if lastErr == nil {
		return errors.New("remote read retry exhausted")
	}
	return fmt.Errorf("remote snapshot did not stabilize after %d retries: %w", maxRetries, lastErr)
}

// remoteCloneRetryConfig reads retry policy from the environment.
func remoteCloneRetryConfig() (maxRetries int, timeout time.Duration) {
	maxRetries = defaultRemoteCloneMaxRetries
	if v := os.Getenv(remoteCloneMaxRetriesEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}

	timeout = defaultRemoteCloneTimeout
	if v := os.Getenv(remoteCloneTimeoutEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}
	return
}

// backoff returns the sleep duration before the next retry attempt.
func backoff(attempt int) time.Duration {
	base := 250 * time.Millisecond
	max := 5 * time.Second
	d := base * (1 << attempt)
	if d > max || d <= 0 {
		return max
	}
	return d
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
