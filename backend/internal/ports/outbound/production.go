package outbound

// ProductionStore is the required cloud capability boundary. Development FS
// and narrowly scoped test doubles need not implement database transactions.
// One instance supplies all ports so callback contexts bind every operation to
// the same transaction; implementing the interface does not itself prove ACID.
type ProductionStore interface {
	GitHubStore
	MetadataStore
	BlobStore
	VerifiedDocStore
	RepositoryStore
	UserPreferencesStore
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
	DocJobStore
	NotificationStore
	RuntimeStore
	StorageAccounting
	SecretsCASStore
	DocReadStore
	AnchoredSessionStore
	BranchLedgerStore
	AnchoredNoteStore
	ReportedWorktreeStore
	RemoteGitStore
	RemotePushStore
	AnchoredPublicationStore
	AnchoredSummaryStore
	AnchoredRemovalStore
}
