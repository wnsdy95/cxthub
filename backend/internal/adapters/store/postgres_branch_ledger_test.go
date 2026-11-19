//go:build postgres

package store

import (
	"testing"
)

func TestPGBranchLedgerPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkBranchLedgerPersistence(t, ctx, st, repo)

	// The tables keep the promises for a writer that skips the application.
	a, c := branchLedgerStoreID("a"), branchLedgerStoreID("c")
	for name, statement := range map[string]string{
		"a second lifetime alive under one name": `UPDATE branch_lifetimes SET closed_at=NULL WHERE repo_id=$1 AND ledger_id='` + a + `'`,
		"an ID in another form":                  `UPDATE branch_lifetimes SET ledger_id='not-an-id' WHERE repo_id=$1 AND ledger_id='` + c + `'`,
		"an empty name":                          `UPDATE branch_lifetimes SET name='' WHERE repo_id=$1 AND ledger_id='` + c + `'`,
		"an ID without its lifetime":             `INSERT INTO branch_ledger_ids(repo_id,id,ledger_id) VALUES($1,'` + branchLedgerStoreID("e") + `','` + branchLedgerStoreID("9") + `')`,
		"an ID given to a second lifetime":       `INSERT INTO branch_ledger_ids(repo_id,id,ledger_id) VALUES($1,'` + a + `','` + c + `')`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the tables", name)
		}
	}
	// Removing a lifetime removes the IDs it held.
	if _, err := st.pool.Exec(ctx, `DELETE FROM branch_lifetimes WHERE repo_id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM branch_ledger_ids WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("IDs outlived their lifetime: %d %v", remaining, err)
	}
}
