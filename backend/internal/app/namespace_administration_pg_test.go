//go:build postgres

package app

import "testing"

func TestPGNamespaceAdministration(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runNamespaceAdministration(t, st)
}
