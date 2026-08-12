package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
)

// Regression test for the server-mode lost-update bug
// (LatentLabsSpace/NEXUS#92): the issue-mutation path used to run
// CALL DOLT_ADD / CALL DOLT_COMMIT inside the still-open SQL transaction.
// DOLT_ADD stages the whole table from the session's BEGIN-time root, so a
// Dolt commit built before the transaction's commit-time merge writes every
// concurrently-committed row back to its BEGIN-time value.
//
// The fix is an ordering contract: the Dolt add/commit MUST execute strictly
// after driver-level tx.Commit(). This test pins that contract with a
// sequence-capturing driver, so it needs no running Dolt server and holds
// for embedded and server mode alike.

// --- sequence-capturing driver --------------------------------------------

type seqRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *seqRecorder) add(ev string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *seqRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

type seqConnector struct{ rec *seqRecorder }

func (c *seqConnector) Connect(context.Context) (driver.Conn, error) {
	return &seqConn{rec: c.rec}, nil
}
func (c *seqConnector) Driver() driver.Driver { return seqDriver{} }

type seqDriver struct{}

func (seqDriver) Open(string) (driver.Conn, error) { return nil, driver.ErrSkip }

type seqConn struct{ rec *seqRecorder }

func (c *seqConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *seqConn) Close() error                        { return nil }
func (c *seqConn) Begin() (driver.Tx, error)           { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *seqConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.rec.add("BEGIN")
	return &seqTx{rec: c.rec}, nil
}

func (c *seqConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.rec.add(query)
	return driver.RowsAffected(1), nil
}

func (c *seqConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.rec.add(query)
	return &seqRows{}, nil
}

type seqTx struct{ rec *seqRecorder }

func (t *seqTx) Commit() error   { t.rec.add("COMMIT"); return nil }
func (t *seqTx) Rollback() error { t.rec.add("ROLLBACK"); return nil }

type seqRows struct{}

func (*seqRows) Columns() []string              { return []string{} }
func (*seqRows) Close() error                   { return nil }
func (*seqRows) Next(dest []driver.Value) error { return io.EOF }

// --- the ordering contract -------------------------------------------------

func TestIssueOperationDoltCommitRunsAfterTxCommit(t *testing.T) {
	rec := &seqRecorder{}
	db := sql.OpenDB(&seqConnector{rec: rec})
	defer func() { _ = db.Close() }()

	s := &DoltStore{db: db}

	err := s.runIssueOperationTx(context.Background(), "bd: update test-1", func(tx *sql.Tx) (storageissueops.ChangedTables, error) {
		if _, err := tx.ExecContext(context.Background(), "UPDATE issues SET status = 'in_progress' WHERE id = 'test-1'"); err != nil {
			return nil, err
		}
		return storageissueops.ChangedTables{"issues": true}, nil
	})
	if err != nil {
		t.Fatalf("runIssueOperationTx: %v", err)
	}

	events := rec.snapshot()
	idx := func(pred func(string) bool) int {
		for i, ev := range events {
			if pred(ev) {
				return i
			}
		}
		return -1
	}

	commitIdx := idx(func(ev string) bool { return ev == "COMMIT" })
	doltAddIdx := idx(func(ev string) bool { return strings.Contains(ev, "DOLT_ADD") })
	doltCommitIdx := idx(func(ev string) bool { return strings.Contains(ev, "DOLT_COMMIT") })

	if commitIdx == -1 {
		t.Fatalf("no driver-level tx COMMIT recorded; events: %v", events)
	}
	if doltAddIdx == -1 || doltCommitIdx == -1 {
		t.Fatalf("DOLT_ADD/DOLT_COMMIT not recorded; events: %v", events)
	}
	if doltAddIdx < commitIdx {
		t.Errorf("DOLT_ADD ran INSIDE the open transaction (index %d < COMMIT index %d) — lost-update ordering regression; events: %v",
			doltAddIdx, commitIdx, events)
	}
	if doltCommitIdx < commitIdx {
		t.Errorf("DOLT_COMMIT ran INSIDE the open transaction (index %d < COMMIT index %d) — lost-update ordering regression; events: %v",
			doltCommitIdx, commitIdx, events)
	}
	if doltCommitIdx < doltAddIdx {
		t.Errorf("DOLT_COMMIT before DOLT_ADD; events: %v", events)
	}

	// No transaction may still be open when the Dolt commit runs: assert no
	// BEGIN after the data COMMIT and before the DOLT_COMMIT.
	for i := commitIdx + 1; i < doltCommitIdx; i++ {
		if events[i] == "BEGIN" {
			t.Errorf("unexpected BEGIN between tx COMMIT and DOLT_COMMIT at index %d; events: %v", i, events)
		}
	}
}

// TestIssueOperationNoTablesSkipsDoltCommit pins the no-op contract: an
// operation reporting no changed tables must not create a Dolt commit.
func TestIssueOperationNoTablesSkipsDoltCommit(t *testing.T) {
	rec := &seqRecorder{}
	db := sql.OpenDB(&seqConnector{rec: rec})
	defer func() { _ = db.Close() }()

	s := &DoltStore{db: db}

	err := s.runIssueOperationTx(context.Background(), "bd: noop", func(tx *sql.Tx) (storageissueops.ChangedTables, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("runIssueOperationTx: %v", err)
	}
	for _, ev := range rec.snapshot() {
		if strings.Contains(ev, "DOLT_COMMIT") {
			t.Errorf("no-op operation produced a DOLT_COMMIT; events: %v", rec.snapshot())
		}
	}
}
