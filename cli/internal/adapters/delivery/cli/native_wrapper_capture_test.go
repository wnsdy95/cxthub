package cli

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestNativeDeferredCaptureUsesOwnedToolThreadNotStaleAffinity(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", "codex")
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	t.Setenv("CXT_WRAPPED_CAPTURE_PROTOCOL", capture.NativeWrapperCaptureProtocol)
	t.Setenv("TERM_SESSION_ID", "stale-terminal")
	const owned = "11111111-1111-4111-8111-111111111111"
	const stale = "22222222-2222-4222-8222-222222222222"
	t.Setenv("CXT_WRAPPED_SESSION_ID", stale)
	t.Setenv("CODEX_THREAD_ID", owned)
	path := writeCodexRollout(t, home, root, owned, time.Now().Add(-time.Hour))
	writeCodexRollout(t, home, root, stale, time.Now())
	retire, err := capture.BindNativeWrapperSession(root, os.Getppid(), owned)
	if err != nil {
		t.Fatal(err)
	}
	defer retire()
	capture.RecordSessionAffinity(root, domain.ProviderCodex, stale)
	got, err := commandCapture(context.Background(), root, "codex")
	if err != nil || got.SessionPath != path {
		t.Fatal("wrong native capture", err)
	}
	if err := retire(); err != nil {
		t.Fatal(err)
	}
	if _, err := commandCapture(context.Background(), root, "codex"); err == nil {
		t.Fatal("missing ownership fell back to previous session")
	}
	t.Setenv("CODEX_THREAD_ID", "")
	if _, err := commandCapture(context.Background(), root, "codex"); err == nil {
		t.Fatal("missing native identity used affinity")
	}
}
