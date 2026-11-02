//go:build postgres

package serverruntime_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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

func childrenRSSBytes(test *testing.T, children ...*executionProcessChild) map[outbound.ProcessRole]int64 {
	test.Helper()
	identifiers := make([]string, 0, len(children))
	roles := make(map[int]outbound.ProcessRole, len(children))
	for _, child := range children {
		identifier := child.command.Process.Pid
		if identifier <= 0 || roles[identifier] != "" {
			test.Fatal("RSS sample requires distinct owned child identifiers")
		}
		roles[identifier] = child.role
		identifiers = append(identifiers, strconv.Itoa(identifier))
	}
	if len(identifiers) == 0 {
		test.Fatal("RSS sample requires an owned child")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=", "-p", strings.Join(identifiers, ",")).Output()
	if err != nil {
		test.Fatal("sample only the owned child RSS", err)
	}
	samples := make(map[outbound.ProcessRole]int64, len(children))
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			test.Fatal("owned child RSS row is not a PID and KiB sample")
		}
		identifier, identifierErr := strconv.Atoi(fields[0])
		kilobytes, memoryErr := strconv.ParseInt(fields[1], 10, 64)
		role := roles[identifier]
		if identifierErr != nil || memoryErr != nil || role == "" || kilobytes <= 0 || samples[role] != 0 {
			test.Fatal("owned child RSS sample has an unknown/duplicate PID or invalid KiB count")
		}
		samples[role] = kilobytes * 1024
	}
	if len(samples) != len(children) {
		test.Fatal("owned child disappeared during the RSS sample")
	}
	return samples
}
