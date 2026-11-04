package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type boundedTagOwnerStore struct {
	*refOwnerStore
}

func (*boundedTagOwnerStore) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("bounded tag owner materialized a snapshot")
}

func (storage *boundedTagOwnerStore) HasOrdinaryTagTarget(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
	storage.probe.touch(ctx, repo, "tag-target")
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := storage.failures["tag-target"]; err != nil {
		return false, err
	}
	_, present := storage.snapshots[target]
	return present, nil
}

func TestRepositoryRefBoundedTagOwnerPreservesSemantics(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		exists    bool
		unchanged bool
		force     bool
		append    bool
		missing   bool
		stale     bool
		authority error
		presence  error
		cas       error
		want      error
		result    inbound.RefUpdateResult
		swaps     int
	}{
		{name: "create", result: inbound.RefFastForward, swaps: 1},
		{name: "append-create", append: true, result: inbound.RefFastForward, swaps: 1},
		{name: "unchanged", exists: true, unchanged: true, result: inbound.RefUpToDate},
		{name: "immutable", exists: true, want: domain.ErrNonFastForward},
		{name: "append-cannot-merge", exists: true, append: true, want: domain.ErrNonFastForward},
		{name: "force", exists: true, force: true, result: inbound.RefForced, swaps: 1},
		{name: "force-append", exists: true, force: true, append: true, result: inbound.RefForced, swaps: 1},
		{name: "stale-force", exists: true, force: true, stale: true, want: domain.ErrRefConflict},
		{name: "missing", missing: true, want: domain.ErrIntegrity},
		{name: "presence-error", presence: domain.ErrConflict, want: domain.ErrConflict},
		{name: "presence-canceled", presence: context.Canceled, want: context.Canceled},
		{name: "cas-error", cas: domain.ErrRefConflict, want: domain.ErrRefConflict, swaps: 1},
		{name: "authority-first", authority: domain.ErrForbidden, want: domain.ErrForbidden},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newRefOwnerFixture(test)
			config := fixture.config()
			config.Store = &boundedTagOwnerStore{refOwnerStore: fixture.store}
			owner := refOwnerCommands(test, config)
			if !owner.SupportsBoundedTagWrites() {
				test.Fatal("bounded store capability was not exposed by its actual owner")
			}
			input := fixture.input()
			input.Ref.Kind, input.Ref.Name, input.Ref.BranchID = domain.RefTag, "release", ""
			input.Force, input.Append = scenario.force, scenario.append
			if scenario.exists {
				fixture.store.refs[refOwnerKey(domain.RefTag, "release")] = domain.Ref{RepoID: input.RepoID, Kind: domain.RefTag, Name: "release", Target: fixture.base}
				input.ExpectedTarget = fixture.base
			}
			if scenario.unchanged {
				input.Ref.Target = fixture.base
			}
			if scenario.missing {
				input.Ref.Target = hh("missing bounded tag target")
			}
			if scenario.stale {
				input.ExpectedTarget = hh("stale bounded tag target")
			}
			fixture.scope.beforeErr = scenario.authority
			fixture.store.failures["tag-target"], fixture.store.failures["cas"] = scenario.presence, scenario.cas
			output, err := owner.UpdateRef(test.Context(), input)
			if !errors.Is(err, scenario.want) || output.Result != scenario.result || len(fixture.store.swaps) != scenario.swaps {
				test.Fatal("bounded tag changed authority, target validation, or CAS", output, err, fixture.store.swaps)
			}
			if err != nil {
				refOwnerAssertZero(test, output)
			}
			lookups := 1
			if scenario.authority != nil {
				lookups = 0
			}
			if fixture.probe.count("tag-target") != lookups || fixture.probe.count("snapshot") != 0 || len(fixture.engine.moves) != 0 ||
				len(fixture.appendPort.writes) != 0 || fixture.probe.count("pendings") != 0 || len(fixture.scope.frames) != 1 {
				test.Fatal("ordinary tag escaped its bounded repository operation", fixture.probe.calls)
			}
			if scenario.swaps != 0 && fixture.store.swaps[0].Expected != input.ExpectedTarget {
				test.Fatal("bounded target check changed the CAS observation", fixture.store.swaps)
			}
		})
	}
}

func TestRepositoryRefBoundedTagCapabilityFallback(test *testing.T) {
	fixture := newRefOwnerFixture(test)
	owner := refOwnerCommands(test, fixture.config())
	if owner.SupportsBoundedTagWrites() || (*RepositoryRefCommands)(nil).SupportsBoundedTagWrites() {
		test.Fatal("missing capability advertised bounded tag writes")
	}
	filesystem := store.NewFSStore(test.TempDir())
	service := NewService(filesystem, filesystem, nil, nil, nil)
	if service.SupportsBoundedTagWrites() || (*Service)(nil).SupportsBoundedTagWrites() {
		test.Fatal("legacy filesystem service advertised bounded tag writes")
	}
	input := fixture.input()
	input.Ref.Kind, input.Ref.Name, input.Ref.BranchID = domain.RefTag, "release", ""
	output, err := owner.UpdateRef(test.Context(), input)
	if err != nil || output.Result != inbound.RefFastForward || fixture.probe.count("snapshot") != 1 {
		test.Fatal("legacy store lost its existing snapshot validation", output, err, fixture.probe.calls)
	}
}
