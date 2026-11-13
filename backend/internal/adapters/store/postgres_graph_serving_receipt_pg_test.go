//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphQualificationReceiptPG(t *testing.T, ctx context.Context, storage *PostgresStore, key domain.GraphQualificationKey) (domain.GraphQualificationResult, domain.GraphProjectionRead) {
	t.Helper()
	read, err := storage.ReadGraphProjection(ctx, key.RepoID)
	if err != nil || read.QualificationKey() != key {
		t.Fatalf("receipt fixture unavailable: %+v %v", read, err)
	}
	digest, err := domain.GraphViewProjectionManifestDigest(ctx, key.RepoID, read.Parts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := domain.VerifyGraphQualificationParts(ctx, key, digest, read.Parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, prepared, err := result.VerifiedParts(); err != nil || !prepared {
		t.Fatalf("receipt not prepared: %v %v", prepared, err)
	}
	return result, read
}

func graphServingHeadCountPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) (heads, parts int) {
	t.Helper()
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM graph_serving_heads WHERE repo_id=$1),
		(SELECT count(*) FROM graph_serving_parts WHERE repo_id=$1)`, string(repo)).Scan(&heads, &parts); err != nil {
		t.Fatal(err)
	}
	return heads, parts
}

func TestPGGraphServingReceiptPromotesVerifiedRows(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result, candidate := graphQualificationReceiptPG(t, ctx, storage, key)
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, key.RepoID)
	if !reflect.DeepEqual(read.Parts, candidate.Parts) || read.Qualification.Actual != result.Actual {
		t.Fatal("receipt promotion did not serve the verified parts")
	}
}

func TestPGGraphServingReceiptCopiesValidatedBytesNotLaterCandidate(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result, candidate := graphQualificationReceiptPG(t, ctx, storage, key)
	graphServingTriggerPG(t, ctx, storage, key.RepoID, "graph_serving_heads",
		`UPDATE graph_projection_parts SET image='{}' WHERE repo_id=NEW.repo_id AND kind='header';`)
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, key.RepoID)
	if !reflect.DeepEqual(read.Parts, candidate.Parts) {
		t.Fatal("receipt promotion copied bytes changed after verification")
	}
}

func TestPGGraphServingReceiptRejectsChangedRowsAtomically(t *testing.T) {
	for name, tamper := range map[string]string{
		"changed": `UPDATE graph_projection_parts SET image='{}' WHERE repo_id=$1 AND kind='header'`,
		"deleted": `UPDATE graph_projection_parts SET deleted=true,image=NULL WHERE repo_id=$1 AND ctid=(SELECT ctid FROM graph_projection_parts
			WHERE repo_id=$1 AND kind<>'header' ORDER BY kind,entity_key LIMIT 1)`,
		"missing": `DELETE FROM graph_projection_parts WHERE repo_id=$1 AND ctid=(SELECT ctid FROM graph_projection_parts
			WHERE repo_id=$1 AND kind<>'header' ORDER BY kind,entity_key LIMIT 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			lease := graphQualificationClaimPG(t, ctx, storage, key)
			result, _ := graphQualificationReceiptPG(t, ctx, storage, key)
			heads, parts := graphServingHeadCountPG(t, ctx, storage, key.RepoID)
			catalogExecPG(t, ctx, storage.pool, tamper, string(key.RepoID))
			if err := storage.FinishGraphQualification(ctx, lease, result); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("tampered rows promoted: %v", err)
			}
			if afterHeads, afterParts := graphServingHeadCountPG(t, ctx, storage, key.RepoID); afterHeads != heads || afterParts != parts {
				t.Fatalf("failed promotion was not atomic: heads %d->%d parts %d->%d", heads, afterHeads, parts, afterParts)
			}
		})
	}
}
