//go:build postgres

package app

import "testing"

func TestActivityCommitHistoryPG(t *testing.T) {
	_, storage, _ := collaborationPG(t)
	checkActivityCommitHistory(t, storage)
}
