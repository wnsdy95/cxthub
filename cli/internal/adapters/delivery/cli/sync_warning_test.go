package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type hookHTTPFailure int

func (e hookHTTPFailure) Error() string {
	return fmt.Sprintf("HTTP status %d; resource 401403", int(e))
}
func (e hookHTTPFailure) StatusCode() int { return int(e) }
func TestSyncWarningDoesNotTreatHashDigitsAsAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		auth bool
	}{
		{"canceled hash", fmt.Errorf("job sha256:401403abc: %w", context.Canceled), false},
		{"deadline hash", fmt.Errorf("doc 401: %w", context.DeadlineExceeded), false},
		{"untyped message", errors.New("failed document 403"), false},
		{"server failure", fmt.Errorf("upload: %w", hookHTTPFailure(500)), false},
		{"unauthorized", fmt.Errorf("upload: %w", hookHTTPFailure(401)), true},
		{"forbidden", fmt.Errorf("upload: %w", hookHTTPFailure(403)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".cxt"), 0700); err != nil {
				t.Fatal(err)
			}
			syncWarn(root, "push", tc.err)
			_, err := os.Stat(filepath.Join(root, ".cxt", "auth-hint-shown"))
			if (err == nil) != tc.auth {
				t.Fatalf("auth marker error=%v, want auth=%v", err, tc.auth)
			}
		})
	}
}
