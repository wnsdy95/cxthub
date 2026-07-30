package cli

import (
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
	provider, managed := supervisedProvider()
	if provider != domain.ProviderCodex || !managed {
		t.Fatalf("provider=%q managed=%v, want codex/true", provider, managed)
	}

	t.Setenv("CXT_WRAPPER_PID", "99999999")
	if _, managed := supervisedProvider(); managed {
		t.Fatal("stale wrapper PID was accepted")
	}

	t.Setenv("CXT_WRAPPER_PID", "")
	if _, managed := supervisedProvider(); managed {
		t.Fatal("missing wrapper PID was accepted")
	}
}
