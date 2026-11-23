//go:build postgres

package serverruntime_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (fixture *executionProcessFixture) activateFullProfile(test *testing.T) {
	test.Helper()
	raw, err := os.ReadFile(fixture.budgetPath)
	if err != nil {
		test.Fatal(err)
	}
	budget, err := executionprofile.Parse(raw)
	if err != nil {
		test.Fatal(err)
	}
	budget.Profile = outbound.BoundedWorkersProfile
	budgetFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal(err)
	}
	fixture.fingerprint, err = store.ProcessAdmissionFingerprint(budgetFingerprint)
	if err != nil {
		test.Fatal(err)
	}
	raw, err = json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(fixture.budgetPath, raw, 0600); err != nil {
		test.Fatal(err)
	}
	fixture.profile = budget.Profile
}

func (child *executionProcessChild) sample(test *testing.T, ctx context.Context) executionProcessEvent {
	test.Helper()
	child.send(test, executionProcessCommand{Operation: "metrics", Token: "owned-resource-sample"})
	statistics := child.expect(test, ctx, "metrics", "owned-resource-sample")
	if statistics.PID != child.command.Process.Pid || statistics.HeapAlloc == 0 || statistics.HeapSys < statistics.HeapAlloc || statistics.RuntimeSys < statistics.HeapSys {
		test.Fatal("owned process returned inconsistent runtime statistics", child.role)
	}
	return statistics
}
