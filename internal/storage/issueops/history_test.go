package issueops

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/types"
)

func TestHistoryInTx_CoalescesNullableHistoricalText(t *testing.T) {
	db, mock, tx := beginMockTx(t)
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() { _ = tx.Rollback() })
	rows := sqlmock.NewRows([]string{"id", "title", "description", "design", "acceptance_criteria", "notes", "status", "priority", "issue_type", "assignee", "owner", "created_by", "estimated_minutes", "created_at", "updated_at", "closed_at", "close_reason", "pinned", "mol_type", "commit_hash", "committer", "commit_date"}).AddRow("id", "title", "", "", "", "", types.StatusOpen, 2, types.TypeTask, nil, nil, nil, nil, "2026-08-29T00:00:00Z", "2026-08-29T00:00:00Z", nil, nil, nil, nil, "hash", "tester", time.Now())
	mock.ExpectQuery(`COALESCE\(description, ''\) AS description`).WithArgs("id").WillReturnRows(rows)
	history, err := HistoryInTx(context.Background(), tx, "id")
	if err != nil {
		t.Fatalf("HistoryInTx: %v", err)
	}
	if len(history) != 1 || history[0].Issue.Description != "" {
		t.Fatalf("unexpected history: %#v", history)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
