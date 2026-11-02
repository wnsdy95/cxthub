//go:build postgres

package store

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayPGSaveRechecksPriorPageAfterLoad(test *testing.T) {
	fixture := docDisplayPG(test,
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, Output: strings.Repeat("a🙂<&>\n", 200)},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, Output: "other"})
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	prepared := fixture.initialize(test, job)
	first := fixture.step(test, prepared)
	if first.Page == nil || len(first.Page.Runs) != domain.DocDisplayPageRuns || first.State.Done {
		test.Fatal("fixture did not produce a bounded nonterminal page")
	}
	prepared, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, first)
	if err != nil {
		test.Fatal(err)
	}
	loaded, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("current checkpoint was not verified before corruption", err)
	}
	next := fixture.step(test, loaded)
	page := first.Objects[len(first.Objects)-1]
	corrupted := bytes.Replace(page.Bytes, []byte(`"source_from":`), []byte(`"source_fram":`), 1)
	if bytes.Equal(corrupted, page.Bytes) || len(corrupted) != len(page.Bytes) || domain.HashContent(corrupted) == page.Hash {
		test.Fatal("fixture did not replace the same-length prior page bytes")
	}
	tag, err := fixture.storage.pool.Exec(fixture.ctx, `WITH replaced AS (
 DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, prepared.State.Source, page.Hash, corrupted)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("prior page was not replaced", err)
	}
	before := fixture.image(test)
	result, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, loaded.Checkpoint, next)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, domain.DocDisplayPreparation{}) {
		test.Fatal("a previous successful decode bypassed current stored bytes", result, err)
	}
	if fixture.image(test) != before {
		test.Fatal("rejected current-byte proof changed durable state")
	}
}
