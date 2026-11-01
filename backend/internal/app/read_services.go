package app

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
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
	DocumentFindFields          documentquery.FindFieldsReader
	DocumentDisplayReads        outbound.DocDisplayReader
	RequireCompleteMemory       bool
}

type ReadServices struct {
	Documents            inbound.DocumentQueries
	DocumentProfile      inbound.DocumentQueryProfile
	Context              inbound.ContextQuery
	ContextProfile       inbound.ContextQueryProfile
	PreparedMemory       inbound.PreparedMemoryReader
	MemorySnapshots      inbound.MemorySnapshotQueries
	MemoryLive           inbound.LiveMemoryQueries
	Archive              inbound.RepositoryArchiveQueries
	GitChanges           inbound.GitChangeQuery
	GitScans             inbound.GitScanQuery
	GitAssociationStatus inbound.GitAssociationStatusQuery
	CodeApplicability    inbound.CodeApplicabilityQuery
	ConversationFind     inbound.ConversationFindQuery
	memoryOwner          *memoryReadOwner
	segmentCache         repositoryquery.SegmentCache
	deliveryCache        memoryquery.DeliveryCache
}

func NewReadServices(config ReadServicesConfig) (*ReadServices, error) {
	if err := validateReadServicesConfig(config); err != nil {
		return nil, err
	}
	transactions, _ := config.Metadata.(documentQuerySnapshots)
	presence, _ := config.Metadata.(outbound.RepositoryTransactionPresence)
	state, _ := config.Metadata.(outbound.RepositoryTransactionState)
	_, transactional := config.Metadata.(outbound.RepositoryTransactions)
	if transactions != nil && !rootAdapterPresent(transactions) || presence != nil && !rootAdapterPresent(presence) || state != nil && !rootAdapterPresent(state) {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.DocumentProfile == documentquery.VerifiedRoots && (transactions == nil || presence == nil) ||
		config.ContextProfile == inbound.ContextQueryPinnedSnapshots && transactions == nil || transactions != nil && presence == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.RootPublicationEnabled && (!transactional || transactions == nil || presence == nil) {
		return nil, domain.ErrRootPublicationDisabled
	}
	supported := append([]domain.DocumentIdentity(nil), config.SupportedDocumentIdentities...)
	services := &ReadServices{DocumentProfile: config.DocumentProfile, ContextProfile: config.ContextProfile, memoryOwner: &memoryReadOwner{}}
	repository := &repositoryQueryScope{metadata: config.Metadata, transactions: transactions, supported: supported}
	documentScope := &documentQueryScope{metadata: config.Metadata, transactions: transactions, presence: presence,
		supported: supported, requireAuthorization: config.DocumentProfile == documentquery.VerifiedRoots}
	documents, err := newReadServicesDocuments(config, documentScope)
	if err != nil {
		return nil, err
	}
	services.Documents, services.ConversationFind = documents, documents
	contributions, err := newReadServicesContributions(config.Metadata)
	if err != nil {
		return nil, err
	}
	graphScope := &legacyGraphProjectionScope{state: state}
	history, _ := config.Metadata.(repositoryquery.HistoryReader)
	revisions, _ := config.Metadata.(repositoryquery.RevisionReader)
	archives, _ := config.Metadata.(repositoryquery.ArchiveReader)
	generations, _ := config.Metadata.(repositoryquery.GraphGenerationReader)
	trees, _ := config.Metadata.(evidencequery.GitTreeReader)
	deltas, _ := config.Metadata.(evidencequery.GitDeltaReader)
	services.Context, err = repositoryquery.NewContext(repositoryquery.ContextConfig{
		Profile: config.ContextProfile, Scope: &contextQueryScope{repository: repository, presence: presence},
		Metadata: config.Metadata, History: history, Revisions: revisions, Archives: archives, Indexes: documents,
		CacheScope: graphScope, Generations: generations, Cache: &services.segmentCache, Contributions: contributions,
		EvidenceMetadata: config.Metadata, Trees: trees, Deltas: deltas, RequireComplete: config.RequireCompleteMemory, AtomicRead: transactions != nil,
	})
	if err != nil {
		return nil, err
	}
	resources := &preparedMemoryQueryScope{ownerKey: services.memoryOwner, transactional: transactional, snapshots: transactions != nil, presence: presence}
	minimumReader, available := config.Metadata.(outbound.MinimumMemoryReadStore)
	minimumProfile := memoryquery.Unavailable
	if available {
		minimumProfile = memoryquery.Available
	}
	minimum, err := memoryquery.NewMinimum(memoryquery.MinimumConfig{
		Profile: minimumProfile, Scope: resources, Reader: minimumReader, SupportedIdentities: supported,
	})
	if err != nil {
		return nil, err
	}
	services.PreparedMemory, err = memoryquery.NewPrepared(memoryquery.PreparedConfig{
		Scope: resources, Minimum: minimum, Repositories: config.PreparedMemoryRepositories,
	})
	if err != nil {
		return nil, err
	}
	snapshotScope := &memorySnapshotScope{resources: resources, repository: repository}
	projection := serviceProjectionSource{MemoryMetadataReader: config.Metadata, MemoryObjectReader: config.Blobs}
	services.MemorySnapshots, err = memoryquery.NewSnapshots(memoryquery.SnapshotConfig{
		Scope: snapshotScope, Metadata: config.Metadata, Objects: config.Blobs, Projection: projection,
	})
	if err != nil {
		return nil, err
	}
	liveHistory, _ := config.Metadata.(memoryquery.LiveHistoryReader)
	liveRevisions, _ := config.Metadata.(memoryquery.LiveRevisionReader)
	liveGenerations, _ := config.Metadata.(memoryquery.LiveGraphGenerationReader)
	memories, _ := config.Metadata.(outbound.RepositoryMemorySourceGenerationStore)
	prPins, _ := config.Metadata.(outbound.PRContributionMemoryReader)
	gitPins, _ := config.Metadata.(outbound.GitContributionMemoryReader)
	if liveRevisions != nil && !rootAdapterPresent(liveRevisions) {
		return nil, domain.ErrCatalogUnsupported
	}
	services.MemoryLive, err = memoryquery.NewLive(memoryquery.LiveConfig{
		Mode: memoryquery.LiveCurrent, Scope: snapshotScope, Metadata: config.Metadata, Projection: projection,
		History: liveHistory, Revisions: memoryLiveRevisions{reader: liveRevisions}, Repository: config.Metadata,
		Evidence: contributions, Trees: trees, Deltas: deltas, GraphScope: graphScope, GraphGenerations: liveGenerations,
		MemoryGenerations: memories, PRPins: prPins, GitPins: gitPins, Delivery: &services.deliveryCache,
		ProofScope: &memoryLiveProofScope{state: state, transactional: transactional}, RequireComplete: config.RequireCompleteMemory,
	})
	if err != nil {
		return nil, err
	}
	archiveScope := &archiveQueryScope{repository: repository, document: documentScope, presence: presence}
	services.Archive, err = newReadServicesArchive(config, archiveScope, transactions != nil)
	if err != nil {
		return nil, err
	}
	services.GitChanges, services.GitScans, services.CodeApplicability, err = newReadServicesEvidenceQueries(config, archiveScope, transactions != nil)
	if err != nil {
		return nil, err
	}
	services.GitAssociationStatus, err = newGitAssociationStatusQueries(config.Metadata, archiveScope, transactions != nil)
	if err != nil {
		return nil, err
	}
	return services, nil
}

func validateReadServicesConfig(config ReadServicesConfig) error {
	if config.DocumentProfile != documentquery.LegacyCompatibility && config.DocumentProfile != documentquery.VerifiedRoots ||
		config.ContextProfile != inbound.ContextQueryCompatibility && config.ContextProfile != inbound.ContextQueryPinnedSnapshots {
		return domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(config.Blobs) {
		return fmt.Errorf("%w: read service metadata and blobs are required", domain.ErrValidation)
	}
	for _, dependency := range []any{config.DocumentFindFields, config.DocumentDisplayReads} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return domain.ErrValidation
		}
	}
	if len(config.SupportedDocumentIdentities) == 0 {
		return domain.ErrValidation
	}
	identities := make(map[domain.DocumentIdentity]bool, len(config.SupportedDocumentIdentities))
	available := ReadDocumentIdentities(config.Metadata, config.Blobs)
	for _, identity := range config.SupportedDocumentIdentities {
		if err := identity.Validate(); err != nil {
			return err
		}
		if identities[identity] {
			return domain.ErrValidation
		}
		if !hasDocumentIdentity(available, identity) {
			return domain.ErrUnsupportedDocumentIdentity
		}
		identities[identity] = true
	}
	if !identities[domain.DocumentIdentityLegacy] {
		return domain.ErrValidation
	}
	if config.DocumentProfile == documentquery.VerifiedRoots && !identities[domain.DocumentIdentityRootV1] {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if err := ValidateConversationRootPublicationConfig(config.RootPublicationEnabled); err != nil {
		return err
	}
	if config.RootPublicationEnabled && !identities[domain.DocumentIdentityRootV1] {
		return domain.ErrRootPublicationDisabled
	}
	if identities[domain.DocumentIdentityRootV1] || config.DocumentProfile == documentquery.VerifiedRoots {
		if !conversationRootReleaseReady {
			return domain.ErrUnsupportedDocumentIdentity
		}
		indexed, _ := config.Blobs.(outbound.DocReadStore)
		verified, _ := config.Blobs.(outbound.VerifiedDocReader)
		events, _ := config.Blobs.(outbound.VerifiedRootEventReader)
		chunks, _ := config.Blobs.(outbound.ConversationChunkReader)
		for _, reader := range []any{indexed, verified, events, chunks} {
			if !rootAdapterPresent(reader) {
				return domain.ErrUnsupportedDocumentIdentity
			}
		}
	}
	if len(config.PreparedMemoryRepositories) > domain.MaxMemoryReadWorkerRepos {
		return domain.ErrValidation
	}
	for _, repo := range config.PreparedMemoryRepositories {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
	}
	return nil
}

func newReadServicesDocuments(config ReadServicesConfig, scope *documentQueryScope) (*documentquery.Queries, error) {
	indexed, _ := config.Blobs.(outbound.DocReadStore)
	verified, _ := config.Blobs.(outbound.VerifiedDocReader)
	events, _ := config.Blobs.(outbound.VerifiedRootEventReader)
	ranges, _ := config.Blobs.(documentquery.DocRangeReader)
	rangeSnapshots, _ := config.Blobs.(outbound.DocRangeSnapshotStore)
	rangeGenerations, _ := config.Blobs.(outbound.DocRangeGenerationStore)
	readiness, _ := config.Blobs.(outbound.DocRangeReadinessStore)
	visible, _ := config.Blobs.(outbound.DocVisibleGenerationStore)
	visibleReadiness, _ := config.Blobs.(documentquery.DocVisibleReadiness)
	boundaries, _ := config.Blobs.(documentquery.DocRangeBoundaryReader)
	chunks, _ := config.Blobs.(outbound.ConversationChunkReader)
	derived, _ := config.Blobs.(outbound.LegacyDerivedByteRangeReader)
	bytes, _ := config.Blobs.(documentquery.DocByteRangeReadiness)
	view := &documentquery.ViewDependencies{Range: ranges, RangeSnapshots: rangeSnapshots, RangeGenerations: rangeGenerations,
		RangeReadiness: readiness, VisibleGenerations: visible, VisibleReadiness: visibleReadiness,
		BoundaryRead: boundaries, Chunks: chunks, DerivedChunks: derived, ByteReadiness: bytes, Resources: scope}
	eventDisplay, _ := config.DocumentDisplayReads.(outbound.DocEventDisplayReader)
	return documentquery.New(documentquery.Config{
		Profile: config.DocumentProfile, ReadOnly: true, Scope: scope, Snapshots: config.Metadata, Legacy: config.Blobs, Index: indexed,
		Verified: verified, RootEvents: events, AtomicRead: scope.transactions != nil, View: view,
		Find: &documentquery.FindDependencies{FieldsRead: config.DocumentFindFields, DisplayRead: config.DocumentDisplayReads, EventDisplay: eventDisplay},
	})
}

func newReadServicesContributions(metadata evidencequery.MetadataReader) (*evidencequery.Queries, error) {
	prLedger, _ := metadata.(evidencequery.PRLedgerReader)
	prHistory, _ := metadata.(evidencequery.PRHistoryReader)
	gitLedger, _ := metadata.(evidencequery.GitLedgerReader)
	gitHeads, _ := metadata.(evidencequery.GitHeadReader)
	gitAdmission, _ := metadata.(evidencequery.GitAdmissionReader)
	gitReceipts, _ := metadata.(evidencequery.GitReceiptReader)
	return evidencequery.New(evidencequery.Config{
		Profile: evidencequery.Compatibility, Metadata: metadata, PRLedger: prLedger, PRHistory: prHistory,
		GitLedger: gitLedger, GitHeads: gitHeads, GitAdmission: gitAdmission, GitReceipts: gitReceipts,
	})
}

func newReadServicesArchive(config ReadServicesConfig, scope archivequery.Scope, atomicRead bool) (inbound.RepositoryArchiveQueries, error) {
	indexed, _ := config.Blobs.(outbound.DocReadStore)
	verified, _ := config.Blobs.(outbound.VerifiedDocReader)
	documents, err := documentquery.NewRawReader(documentquery.RawConfig{
		Profile: config.DocumentProfile, Snapshots: config.Metadata, Legacy: config.Blobs, Index: indexed, Verified: verified,
	})
	if err != nil {
		return nil, err
	}
	candidates, _ := config.Blobs.(archivequery.CandidateReader)
	return archivequery.New(archivequery.Config{
		Profile: config.DocumentProfile, Scope: scope, Metadata: config.Metadata,
		Documents: documents, Candidates: candidates, AtomicRead: atomicRead,
	})
}

func newReadServicesEvidenceQueries(config ReadServicesConfig, scope evidencequery.Scope, atomicRead bool) (inbound.GitChangeQuery, inbound.GitScanQuery, inbound.CodeApplicabilityQuery, error) {
	changes, _ := config.Metadata.(evidencequery.ChangeReader)
	changeQueries, err := evidencequery.NewChangeQueries(evidencequery.ChangeConfig{
		Scope: scope, Changes: changes, AtomicRead: atomicRead,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	scans, _ := config.Metadata.(evidencequery.ScanReader)
	heads, _ := config.Metadata.(evidencequery.HeadScanReader)
	scanQueries, err := evidencequery.NewScanQueries(evidencequery.ScanConfig{
		Scope: scope, Metadata: config.Metadata, Scans: scans, Heads: heads, AtomicRead: atomicRead,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	revisions, _ := config.Metadata.(evidencequery.RevisionReader)
	trees, _ := config.Metadata.(evidencequery.GitTreeReader)
	deltas, _ := config.Metadata.(evidencequery.GitDeltaReader)
	codeQueries, err := evidencequery.NewCodeQueries(evidencequery.CodeConfig{
		Scope: scope, Metadata: config.Metadata, Revisions: revisions, Trees: trees, Deltas: deltas,
		RequireComplete: config.RequireCompleteMemory, AtomicRead: atomicRead,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return changeQueries, scanQueries, codeQueries, nil
}
