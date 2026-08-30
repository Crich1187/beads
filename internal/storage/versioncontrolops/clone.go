package versioncontrolops

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/steveyegge/beads/internal/doltretry"
)

// DoltClone clones a Dolt database from a remote URL.
// conn must be a non-transactional database connection.
// The database parameter specifies the local database name for the clone.
// If user is non-empty, authenticates with that user. Dolt reads the remote
// password from DOLT_REMOTE_PASSWORD.
//
// The call is retried when Dolt reports a transient consistency error (e.g., a
// manifest that references table files not yet visible due to a concurrent
// push). This mirrors the read-side retry in the CLI clone path.
func DoltClone(ctx context.Context, conn DBConn, remoteURL, database, user string) error {
	query := "CALL DOLT_CLONE(?, ?)"
	args := []any{remoteURL, database}
	if user != "" {
		query = "CALL DOLT_CLONE('--user', ?, ?, ?)"
		args = []any{user, remoteURL, database}
	}

	cloneFn := func() error {
		// GH#4272: the initial fetch runs git hooks just like push/pull; disable
		// them for the clone window too (see remotes.go for the full rationale).
		return withRemoteEnvGuards(func() error {
			if _, err := conn.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("dolt clone %s: %w", sanitizeURL(remoteURL), err)
			}
			return nil
		})
	}

	// Fast path: no retry needed in the common case.
	if err := cloneFn(); err == nil {
		return nil
	} else if !doltretry.IsRemoteCloneTransientError(err) {
		return err
	}

	// Slow path: the remote snapshot may be mid-update. Back off and retry.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		deadline, _ = ctx.Deadline()
	}

	for attempt := 0; ; attempt++ {
		err := cloneFn()
		if err == nil {
			return nil
		}
		if !doltretry.IsRemoteCloneTransientError(err) {
			return err
		}
		wait := 250 * time.Millisecond * (1 << attempt)
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		if time.Until(deadline) <= wait {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// sanitizeURL removes credentials from a URL for safe error reporting.
func sanitizeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
