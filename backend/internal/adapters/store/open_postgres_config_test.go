//go:build postgres

package store

import (
	"strings"
	"testing"
)

func TestOpenRequiresDSNWhenPostgresIsEnforced(t *testing.T) {
	_, err := Open(t.TempDir(), "", true)
	if err == nil || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN is required") {
		t.Fatalf("Open error = %v, want missing DSN failure", err)
	}
}
