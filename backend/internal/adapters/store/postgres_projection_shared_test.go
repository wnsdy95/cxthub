//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestProjectionFamiliesKeepScopeAndEvidenceSeparate(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	pending := (*PostgresStore)(nil).pendingProjection()
	graph := (*PostgresStore)(nil).graphProjection()
	if err := graph.validatePage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("graph accepted pending page: %v", err)
	}
	checkpoint := graph.checkpoint(work.RepoID, work.Epoch, page.Through)
	page.Scope, page.Checkpoint = checkpoint.Scope, &checkpoint
	page.Entries = []domain.RepositoryViewSourceEntry{{Kind: "git_tree", Key: "tree", Sequence: 5, Value: json.RawMessage(`{"payload":{}}`)}}
	if err := graph.validatePage(work, source, page); err != nil {
		t.Fatal(err)
	}
	if err := pending.validatePage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("pending accepted graph page: %v", err)
	}
	page.Scope = pending.spec.scope
	pendingCheckpoint := pending.checkpoint(work.RepoID, work.Epoch, page.Through)
	page.Checkpoint = &pendingCheckpoint
	if err := pending.validatePage(work, source, page); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("pending accepted graph evidence under view scope: %v", err)
	}
}

func TestProjectionRejectsOversizeBeforeParsing(t *testing.T) {
	work, source, page := pendingProjectionValidationFixture()
	page.Entries[0].Value = json.RawMessage(strings.Repeat("[", domain.MaxRepositoryViewSourceImageBytes+1))
	if err := validatePendingProjectionPage(work, source, page); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("oversize malformed source was parsed: %v", err)
	}
	oversized := []domain.PendingProjectionPart{{Kind: "field", Key: "oversized",
		Value: json.RawMessage(strings.Repeat("[", domain.MaxPendingProjectionOutputBytes+1))}}
	for _, projection := range []projectionStore{(*PostgresStore)(nil).pendingProjection(), (*PostgresStore)(nil).graphProjection()} {
		if err := projection.validateParts(context.Background(), work.RepoID, oversized); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
			t.Fatalf("%s parsed oversize output: %v", projection.spec.scope, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := projection.validateParts(ctx, work.RepoID, oversized); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s ignored cancellation: %v", projection.spec.scope, err)
		}
	}
}
