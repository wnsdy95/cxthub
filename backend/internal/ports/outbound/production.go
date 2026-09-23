package outbound

// ProductionStore is the required cloud capability boundary. Development FS
// and narrowly scoped test doubles need not implement database transactions.
// One instance supplies all ports so callback contexts bind every operation to
// the same transaction; implementing the interface does not itself prove ACID.
type ProductionStore interface {
	MetadataStore
	BlobStore
	VerifiedDocStore
	RepositoryStore
	OrganizationStore
	RepositoryOrganizationAccess
	EnterpriseStore
	TeamStore
	IdentityTransactions
	RepositoryBindings
	OAuthStore
	RepositoryTransactions
	RepositoryMetadataTransactions
	RepositoryAccessLocker
	RepositoryRevisions
	EvidenceRevisions
	HistoryStore
	PRJobStore
	PRJobFence
	DocJobStore
	NotificationStore
	RuntimeStore
	StorageAccounting
	SecretsCASStore
	GitChangeStore
	GitScanStore
	GitHeadScanStore
	GitTreeStore
	DocReadStore
}
