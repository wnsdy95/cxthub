//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type boundedTagOwnerPG struct {
	*store.PostgresStore
	lookups int
}

func (*boundedTagOwnerPG) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("PostgreSQL bounded tag owner materialized a snapshot")
}

func (storage *boundedTagOwnerPG) HasOrdinaryTagTarget(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
	storage.lookups++
	return storage.PostgresStore.HasOrdinaryTagTarget(ctx, repo, target)
}

func TestPGRepositoryRefBoundedTagOwnerAuthorityAndCAS(test *testing.T) {
	fixture := newRepositoryRefFixturePG(test)
	probe := &boundedTagOwnerPG{PostgresStore: fixture.storage}
	owner := repositoryRefOwnerPG(test, NewService(probe, probe, nil, gitengine.NewEngine(probe), probe))
	service := NewService(probe, probe, nil, gitengine.NewEngine(probe), probe)
	if !owner.SupportsBoundedTagWrites() || !service.SupportsBoundedTagWrites() {
		test.Fatal("PostgreSQL owner did not expose bounded ordinary tags")
	}
	if present, err := probe.HasOrdinaryTagTarget(fixture.ctx, hh("foreign bounded tag repository"), fixture.base); err != nil || present {
		test.Fatal("bounded target presence escaped its repository", present, err)
	}
	for _, scenario := range []struct {
		name      string
		exists    bool
		force     bool
		append    bool
		stale     bool
		missing   bool
		forbidden bool
		legacy    bool
		want      error
		result    inbound.RefUpdateResult
	}{
		{name: "create", result: inbound.RefFastForward},
		{name: "legacy-create", legacy: true, result: inbound.RefFastForward},
		{name: "append-create", append: true, result: inbound.RefFastForward},
		{name: "force", exists: true, force: true, result: inbound.RefForced},
		{name: "legacy-force", legacy: true, exists: true, force: true, result: inbound.RefForced},
		{name: "immutable-append", exists: true, append: true, want: domain.ErrNonFastForward},
		{name: "stale-force", exists: true, force: true, stale: true, want: domain.ErrRefConflict},
		{name: "missing-target", missing: true, want: domain.ErrIntegrity},
		{name: "authority-first", forbidden: true, want: domain.ErrForbidden},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			input := inbound.UpdateRefInput{RepoID: fixture.repo,
				Ref:   domain.Ref{Kind: domain.RefTag, Name: "bounded/" + scenario.name, Target: fixture.tip},
				Force: scenario.force, Append: scenario.append}
			if scenario.exists {
				initial := input.Ref
				initial.Target = fixture.base
				if err := fixture.storage.CompareAndSwapRef(fixture.ctx, fixture.repo, initial, ""); err != nil {
					test.Fatal(err)
				}
				input.ExpectedTarget = fixture.base
			}
			if scenario.stale {
				input.ExpectedTarget = fixture.orphan
			}
			if scenario.missing {
				input.Ref.Target = hh("absent bounded tag snapshot")
			}
			actor := repositoryRefActorPG(fixture)
			if scenario.forbidden {
				actor = inbound.WithRepositoryActor(fixture.ctx, fixture.team.outsider.ID)
			}
			before, lookups := fixture.image(test), probe.lookups
			var commands inbound.RepositoryRefCommands = owner
			if scenario.legacy {
				commands = service
			}
			output, err := commands.UpdateRef(actor, input)
			if !errors.Is(err, scenario.want) || output.Result != scenario.result {
				test.Fatal("bounded PostgreSQL tag changed authority or CAS semantics", output, err, scenario.want)
			}
			after := fixture.image(test)
			wantLookups := 1
			if scenario.forbidden {
				wantLookups = 0
			}
			if probe.lookups-lookups != wantLookups {
				test.Fatal("tag target lookup bypassed authority or repeated its probe", probe.lookups-lookups)
			}
			if err != nil {
				refOwnerAssertZero(test, output)
				if !reflect.DeepEqual(after, before) {
					test.Fatal("rejected tag published durable effects")
				}
				return
			}
			if after.revision.Graph != before.revision.Graph+1 || after.revision.Evidence != before.revision.Evidence ||
				after.revision.Pending != before.revision.Pending || len(after.audit) != len(before.audit)+1 ||
				!reflect.DeepEqual(after.notifications, before.notifications) || !reflect.DeepEqual(after.pending, before.pending) ||
				!reflect.DeepEqual(after.snapshots, before.snapshots) || !reflect.DeepEqual(after.history, before.history) {
				test.Fatal("bounded tag changed revisions, notifications, history, or snapshots")
			}
			ref, err := fixture.storage.GetRef(fixture.ctx, fixture.repo, domain.RefTag, input.Ref.Name)
			if err != nil || ref.Target != input.Ref.Target {
				test.Fatal("bounded tag did not persist its CAS", ref, err)
			}
		})
	}
}
