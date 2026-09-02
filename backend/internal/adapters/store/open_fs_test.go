//go:build !postgres

package store

import (
	"strings"
	"testing"
)

func TestOpenFailsClosedWhenPostgresIsRequestedWithoutAdapter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		dsn             string
		requirePostgres bool
	}{
		{name: "dsn cannot be ignored", dsn: "postgres://example.invalid/cxt"},
		{name: "production requirement", requirePostgres: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(t.TempDir(), tc.dsn, tc.requirePostgres)
			if err == nil || !strings.Contains(err.Error(), "without the postgres tag") {
				t.Fatalf("Open error = %v, want explicit postgres build failure", err)
			}
		})
	}
}

func TestOpenAllowsFilesystemOnlyWhenPostgresIsNotRequested(t *testing.T) {
	st, err := Open(t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*FSStore); !ok {
		t.Fatalf("Open returned %T, want *FSStore", st)
	}
}
