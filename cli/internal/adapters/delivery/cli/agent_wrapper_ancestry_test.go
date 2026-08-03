package cli

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestHasProcessAncestor(t *testing.T) {
	parents := map[int]int{50: 40, 40: 30, 30: 1, 70: 60, 60: 1}
	parent := func(pid int) (int, bool) {
		ppid, ok := parents[pid]
		return ppid, ok
	}
	if !hasProcessAncestor(50, 30, parent) {
		t.Fatal("owning wrapper ancestor was not recognized")
	}
	if hasProcessAncestor(50, 60, parent) {
		t.Fatal("unrelated live wrapper was accepted")
	}
	if hasProcessAncestor(50, 99, parent) {
		t.Fatal("missing/stale wrapper was accepted")
	}
	if hasProcessAncestor(1, 1, parent) {
		t.Fatal("init PID must never be accepted as a wrapper")
	}
}

func TestHasProcessAncestorFailsClosedOnCycle(t *testing.T) {
	parent := func(pid int) (int, bool) {
		if pid == 10 {
			return 20, true
		}
		return 10, true
	}
	if hasProcessAncestor(10, 99, parent) {
		t.Fatal("cyclic process ancestry accepted an unrelated wrapper")
	}
}

func TestSupervisedProviderRequiresOwningWrapperPID(t *testing.T) {
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", "codex")
	t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
	provider, managed := supervisedProvider(context.Background(), t.TempDir())
	if provider != domain.ProviderCodex || !managed {
		t.Fatalf("provider=%q managed=%v, want codex/true", provider, managed)
	}

	t.Setenv("CXT_WRAPPER_PID", "99999999")
	if _, managed := supervisedProvider(context.Background(), t.TempDir()); managed {
		t.Fatal("stale wrapper PID was accepted")
	}

	t.Setenv("CXT_WRAPPER_PID", "")
	if _, managed := supervisedProvider(context.Background(), t.TempDir()); managed {
		t.Fatal("missing wrapper PID was accepted")
	}
}

func TestProviderByRecency(t *testing.T) {
	if providerByRecency(0, 0) != domain.ProviderClaude {
		t.Fatal("no sessions must default to claude")
	}
	if providerByRecency(100, 200) != domain.ProviderCodex {
		t.Fatal("newer codex session was not selected")
	}
	if providerByRecency(200, 100) != domain.ProviderClaude {
		t.Fatal("newer claude session was not selected")
	}
	if providerByRecency(100, 100) != domain.ProviderClaude {
		t.Fatal("tie must stay claude")
	}
}

func TestParsePIDTable(t *testing.T) {
	table := parsePIDTable([]byte("  50   40\n40 30\ngarbage line here\n 30    1\n\n"))
	if len(table) != 3 || table[50] != 40 || table[40] != 30 || table[30] != 1 {
		t.Fatalf("parsed table = %v", table)
	}
}
