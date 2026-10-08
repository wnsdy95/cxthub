package serverruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP6RootAdmissionRejectsIncompleteRuntime(t *testing.T) {
	t.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "true")
	t.Setenv("CXT_AUTH", "dev")
	t.Setenv("CXT_PUBLIC_URL", "http://localhost:8080")
	t.Setenv("CXT_POSTGRES_DSN", "")
	dir := filepath.Join(t.TempDir(), "must-not-be-created")
	// FS has complete readers, but cannot claim production transactions for
	// enabling new admission. This remains invalid after binary release.
	runtime, err := Open(context.Background(), "127.0.0.1:8080", dir, false)
	if !errors.Is(err, domain.ErrRootPublicationDisabled) || runtime != nil {
		t.Fatal("incomplete runtime admitted root publication", runtime, err)
	}
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed startup touched store", err)
	}
}

func TestP6InvalidConfigurationBeforeRuntimeEffects(t *testing.T) {
	t.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
	t.Setenv("CXT_AUTH", "invalid-auth-mode")
	t.Setenv("CXT_POSTGRES_DSN", "")
	dir := filepath.Join(t.TempDir(), "must-not-be-created")
	runtime, err := Open(context.Background(), "127.0.0.1:8080", dir, false)
	if err == nil || !strings.Contains(err.Error(), "CXT_AUTH must be firebase or dev") || runtime != nil {
		t.Fatal("invalid configuration passed early validation", runtime, err)
	}
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid config touched store", err)
	}
}
