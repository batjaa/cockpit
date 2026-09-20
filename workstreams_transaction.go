package main

import (
	"context"
	"database/sql"
)

// beginWrite reserves SQLite's single writer before reading a mutation's
// snapshot. busy_timeout cannot retry a WAL read-to-write snapshot upgrade
// after another writer (including PR discovery) commits. The zero-row UPDATE
// acquires that reservation without changing records, revisions, or history;
// readers remain concurrent under WAL, and file I/O stays outside this scope.
func (st *WorkstreamStore) beginWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workstreams SET revision=revision WHERE 0`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
