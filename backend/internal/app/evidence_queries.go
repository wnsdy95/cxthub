package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) evidenceQueries() (*evidencequery.Queries, error) {
	return newEvidenceQueries(service.graphEvidenceReader(), service.projectionEvidence != nil)
}

func newEvidenceQueries(reader outbound.RepositoryReader, captured bool) (*evidencequery.Queries, error) {
	config := evidencequery.Config{Profile: evidencequery.Compatibility, Metadata: reader}
	if captured {
		config.Profile = evidencequery.CompleteEvidence
	}
	config.PRLedger, _ = reader.(evidencequery.PRLedgerReader)
	config.PRHistory, _ = reader.(evidencequery.PRHistoryReader)
	config.GitLedger, _ = reader.(evidencequery.GitLedgerReader)
	config.GitHeads, _ = reader.(evidencequery.GitHeadReader)
	config.GitAdmission, _ = reader.(evidencequery.GitAdmissionReader)
	config.GitReceipts, _ = reader.(evidencequery.GitReceiptReader)
	return evidencequery.New(config)
}
