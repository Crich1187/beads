package uow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/domain/db"
)

type doltServerTx struct {
	conn *sql.Conn
	done bool
}

var _ Tx = (*doltServerTx)(nil)

func (t *doltServerTx) Runner() db.Runner {
	return t.conn
}

func (t *doltServerTx) Commit(ctx context.Context, message string) error {
	if t.done {
		return errors.New("uow: commit: already done")
	}
	t.done = true
	defer t.releaseConn()

	// Explicitly stage config first. Under dolt sql-server, DOLT_COMMIT('-Am')
	// can return success while leaving config unstaged, silently stranding
	// `bd config set` in the working set (root-c1q3p). Staging via DOLT_ADD
	// before commit is the proven durable path.
	if _, err := t.conn.ExecContext(ctx, "CALL DOLT_ADD('config')"); err != nil {
		// Config may already be clean / not dirty — continue and stage others.
		_ = err
	}

	rows, err := t.conn.QueryContext(ctx, "SELECT table_name FROM dolt_status")
	if err != nil {
		return fmt.Errorf("uow: query dolt_status: %w", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			return fmt.Errorf("uow: scan dolt_status: %w", err)
		}
		if table == "config" {
			continue // already staged above
		}
		tables = append(tables, table)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("uow: iterate dolt_status: %w", err)
	}

	for _, table := range tables {
		if _, err := t.conn.ExecContext(ctx, "CALL DOLT_ADD(?)", table); err != nil {
			// Best effort: some tables may be dolt_ignore'd (e.g., wisps).
			continue
		}
	}

	_, err = t.conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', ?);", message)
	return err
}

func (t *doltServerTx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.releaseConn()
	_, err := t.conn.ExecContext(ctx, "ROLLBACK;")
	return err
}

func (t *doltServerTx) RollbackUnlessCommitted(ctx context.Context) {
	if !t.done {
		_ = t.Rollback(ctx)
	}
}

func (t *doltServerTx) releaseConn() {
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
}
