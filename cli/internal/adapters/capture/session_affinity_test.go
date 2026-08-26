package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestSessionAffinityIsTerminalAndProviderScoped(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-a")
	RecordSessionAffinity(repo, domain.ProviderCodex, "11111111-1111-4111-8111-111111111111")
	if got := SessionAffinity(repo, domain.ProviderCodex); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("same terminal affinity = %q", got)
	}
	if got := SessionAffinity(repo, domain.ProviderClaude); got != "" {
		t.Fatalf("provider leaked affinity = %q", got)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-b")
	if got := SessionAffinity(repo, domain.ProviderCodex); got != "" {
		t.Fatalf("terminal leaked affinity = %q", got)
	}
}
