package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ReadServicesConfig struct {
	Metadata                    outbound.MetadataStore
	Blobs                       outbound.BlobStore
	DocumentProfile             inbound.DocumentQueryProfile
	ContextProfile              inbound.ContextQueryProfile
	SupportedDocumentIdentities []domain.DocumentIdentity
	RootPublicationEnabled      bool
	PreparedMemoryRepositories  []domain.ContentHash
	RequireCompleteMemory       bool
}

func newReadServicesDocuments(config ReadServicesConfig, scope *documentQueryScope) (*documentquery.Queries, error) {
	indexed, _ := config.Blobs.(outbound.DocReadStore)
	verified, _ := config.Blobs.(outbound.VerifiedDocReader)
	events, _ := config.Blobs.(outbound.VerifiedRootEventReader)
	ranges, _ := config.Blobs.(documentquery.DocRangeReader)
	rangeSnapshots, _ := config.Blobs.(outbound.DocRangeSnapshotStore)
	rangeGenerations, _ := config.Blobs.(outbound.DocRangeGenerationStore)
	readiness, _ := config.Blobs.(outbound.DocRangeReadinessStore)
	visibleReadiness, _ := config.Blobs.(documentquery.DocVisibleReadiness)
	boundaries, _ := config.Blobs.(documentquery.DocRangeBoundaryReader)
	chunks, _ := config.Blobs.(outbound.ConversationChunkReader)
	derived, _ := config.Blobs.(outbound.LegacyDerivedByteRangeReader)
	bytes, _ := config.Blobs.(documentquery.DocByteRangeReadiness)
	view := &documentquery.ViewDependencies{Range: ranges, RangeSnapshots: rangeSnapshots, RangeGenerations: rangeGenerations,
		RangeReadiness: readiness, VisibleReadiness: visibleReadiness,
		BoundaryRead: boundaries, Chunks: chunks, DerivedChunks: derived, ByteReadiness: bytes, Resources: scope}
	return documentquery.New(documentquery.Config{
		Profile: config.DocumentProfile, ReadOnly: true, Scope: scope, Snapshots: config.Metadata, Legacy: config.Blobs, Index: indexed,
		Verified: verified, RootEvents: events, AtomicRead: scope.transactions != nil, View: view,
	})
}
