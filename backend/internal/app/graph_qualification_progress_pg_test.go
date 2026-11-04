//go:build postgres

package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphQualificationMakesProgressWhileSourceKeepsChanging(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := worker.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	var first domain.GraphProjectionRead
	for step := range 12 {
		if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal("projection", step, err)
		}
		before, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.main.Target), fmt.Sprintf("continuous write %d", step)); err != nil {
			test.Fatal(err)
		}
		after, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
		if err != nil || after.Sequence <= before.Sequence {
			test.Fatal("fixture did not advance source before qualification", err)
		}
		if err := worker.ProcessGraphQualifications(ctx, 1); err != nil {
			test.Fatal("qualification", step, err)
		}
		served, err := storage.ReadQualifiedGraphProjection(ctx, repo)
		if err != nil {
			test.Fatal("read", step, err)
		}
		if served.Ready {
			test.Fatal("fixture accidentally allowed a quiet latest source")
		}
		if !served.Complete {
			continue
		}
		if served.Qualification == nil || !served.Qualification.Matched() {
			test.Fatal("unqualified generation counted as progress")
		}
		if first.Dataset == "" {
			first = served
		} else if served.Applied.Epoch == first.Applied.Epoch && served.Applied.Sequence > first.Applied.Sequence {
			return
		}
	}
	if first.Dataset == "" {
		test.Fatal("no first qualified graph while writes continued")
	}
	test.Fatal("qualified graph did not advance while writes continued")
}
