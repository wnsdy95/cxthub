//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationParserBindingMatchesSharedDocumentScope(test *testing.T) {
	work := &legacyNormalizationCheckpointWork{job: domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte("repo-a")), DocHash: domain.HashContent([]byte("shared document"))},
		state: legacyByteCheckpoint{stage: legacyByteStage{sourceID: "source", sourceVersion: 3, incarnation: "stage"}}}
	binding := work.parserBinding()
	work.job.RepoID = domain.HashContent([]byte("repo-b"))
	if work.parserBinding() != binding {
		test.Fatal("repository-local authorization was mixed into the globally shared document checkpoint identity")
	}
	work.state.stage.sourceVersion++
	if work.parserBinding() == binding {
		test.Fatal("parser checkpoint identity did not follow current source generation")
	}
}
