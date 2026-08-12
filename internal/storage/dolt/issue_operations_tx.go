package dolt

import (
	"context"
	"database/sql"
	"sort"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
)

func (s *DoltStore) runIssueOperationTx(ctx context.Context, commitMsg string, fn func(*sql.Tx) (storageissueops.ChangedTables, error)) error {
	return s.runIssueOperationTxWithMessage(ctx, func(tx *sql.Tx) (storageissueops.ChangedTables, string, error) {
		tables, err := fn(tx)
		return tables, commitMsg, err
	})
}

// runIssueOperationTxWithMessage is runIssueOperationTx for an operation whose
// commit message is only known once the body has run. A ready claim names the
// id it won, and nothing outside the transaction can predict which one that
// is, so the message is composed where the selection happens.
//
// Ordering contract (lost-update fix, LatentLabsSpace/NEXUS#92): the Dolt
// commit MUST run after the SQL transaction commits, never inside it.
// DOLT_ADD stages the ENTIRE table from the session's transaction root; when
// it ran inside the still-open transaction, the Dolt commit was built from
// the pre-merge BEGIN-time snapshot with the then-current branch HEAD as its
// parent — so every row a concurrent writer had committed inside the
// transaction's window was silently written back to its BEGIN-time value
// (observed in production as reverted claims). Committing the SQL
// transaction first lets Dolt's commit-time three-way merge reconcile this
// session's changes with concurrent writers; the post-commit DOLT_ADD then
// stages the merged state, which cannot resurrect stale rows.
//
// Failure mode note: if the post-commit Dolt commit fails, the data change
// HAS landed — it remains in the branch working set and rides the next Dolt
// commit. Callers must not treat that error as "nothing happened".
func (s *DoltStore) runIssueOperationTxWithMessage(ctx context.Context, fn func(*sql.Tx) (storageissueops.ChangedTables, string, error)) error {
	var staged []string
	var commitMsg string
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		// Reset on every attempt: a retried transaction re-runs the body and
		// must not accumulate tables from a rolled-back attempt.
		staged, commitMsg = nil, ""
		tables, msg, err := fn(tx)
		if err != nil {
			return err
		}
		for table := range tables {
			staged = append(staged, table)
		}
		sort.Strings(staged)
		commitMsg = msg
		return nil
	})
	if err != nil {
		return err
	}
	if len(staged) == 0 {
		return nil
	}
	return s.doltAddAndCommitPostTx(ctx, staged, commitMsg)
}
