package app

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// ValidateProductionStore runs before migrations/workers/listening. Optional
// test/development fallbacks must never silently remove a cloud guarantee.
func ValidateProductionStore(store any) error {
	if store == nil {
		return fmt.Errorf("production store is nil")
	}
	value := reflect.ValueOf(store)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return fmt.Errorf("production store is nil")
	}
	if _, ok := store.(outbound.ProductionStore); ok {
		return nil
	}
	required := []struct {
		name    string
		present bool
	}{
		{"metadata", supports[outbound.MetadataStore](store)},
		{"blobs", supports[outbound.BlobStore](store)},
		{"verified document publication", supports[outbound.VerifiedDocStore](store)},
		{"repository", supports[outbound.RepositoryStore](store)},
		{"atomic user preferences", supports[outbound.UserPreferencesStore](store)},
		{"organization", supports[outbound.OrganizationStore](store)},
		{"teams", supports[outbound.TeamStore](store)},
		{"enterprise accounts", supports[outbound.EnterpriseStore](store)},
		{"identity transactions", supports[outbound.IdentityTransactions](store)},
		{"repository bindings", supports[outbound.RepositoryBindings](store)},
		{"OAuth", supports[outbound.OAuthStore](store)},
		{"repository write/read transactions", supports[outbound.RepositoryTransactions](store)},
		{"repository transactions", supports[outbound.RepositoryMetadataTransactions](store)},
		{"repository access locks", supports[outbound.RepositoryAccessLocker](store)},
		{"repository revisions", supports[outbound.RepositoryRevisions](store)},
		{"evidence revisions", supports[outbound.EvidenceRevisions](store)},
		{"document jobs", supports[outbound.DocJobStore](store)},
		{"notification outbox", supports[outbound.NotificationStore](store)},
		{"shared authentication/rate limits", supports[outbound.RuntimeStore](store)},
		{"storage accounting", supports[outbound.StorageAccounting](store)},
		{"secrets CAS", supports[outbound.SecretsCASStore](store)},
		{"indexed document reads", supports[outbound.DocReadStore](store)},
		{"anchored session reports", supports[outbound.AnchoredSessionStore](store)},
		{"branch ledger", supports[outbound.BranchLedgerStore](store)},
		{"manual notes", supports[outbound.AnchoredNoteStore](store)},
		{"reported worktrees", supports[outbound.ReportedWorktreeStore](store)},
		{"remote Git cache", supports[outbound.RemoteGitStore](store)},
		{"remote push record", supports[outbound.RemotePushStore](store)},
		{"session publications", supports[outbound.AnchoredPublicationStore](store)},
		{"session summaries", supports[outbound.AnchoredSummaryStore](store)},
		{"session removals", supports[outbound.AnchoredRemovalStore](store)},
	}
	var missing []string
	for _, capability := range required {
		if !capability.present {
			missing = append(missing, capability.name)
		}
	}
	return fmt.Errorf("production store lacks required capabilities: %s", strings.Join(missing, ", "))
}
func supports[T any](value any) bool { _, ok := value.(T); return ok }
