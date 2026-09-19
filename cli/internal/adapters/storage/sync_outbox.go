package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"sort"
	"syscall"
	"time"
)

// SyncOutboxAdapter persists retry work without owning graft or ref policy.
type SyncOutboxAdapter struct{}

func NewSyncOutbox() *SyncOutboxAdapter { return &SyncOutboxAdapter{} }
func (*SyncOutboxAdapter) withLock(ctx context.Context, root, name string, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := NewFileStore(root).withOSLock(ctx, "sync-outbox", name, syscall.LOCK_EX, true, fn)
	return err
}

type graftQueueAccess struct {
	root   string
	active bool
}

func (q *graftQueueAccess) Load() ([]domain.GraftQueueEvent, error) {
	if !q.active {
		return nil, fmt.Errorf("graft queue access outside transaction")
	}
	state, err := readGraftQueue(q.root, ".cxt/grafts.json")
	return state.Events, err
}
func (q *graftQueueAccess) Store(events []domain.GraftQueueEvent) error {
	if !q.active {
		return fmt.Errorf("graft queue access outside transaction")
	}
	return writeGraftQueue(q.root, ".cxt/grafts.json", graftQueueState{Events: events})
}
func (s *SyncOutboxAdapter) WithGrafts(ctx context.Context, root string, fn func(outbound.GraftQueueAccess) error) error {
	return s.withLock(ctx, root, "grafts", func() error {
		q := &graftQueueAccess{root: root, active: true}
		defer func() { q.active = false }()
		return fn(q)
	})
}

type graftQueueState struct {
	Version int                      `json:"version"`
	Events  []domain.GraftQueueEvent `json:"events"`
}

const graftQueueVersion = 1

func readGraftQueue(repoRoot, rel string) (graftQueueState, error) {
	state := graftQueueState{Version: graftQueueVersion}
	b, err := providerfs.ReadRepoFile(repoRoot, rel)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	var current graftQueueState
	if json.Unmarshal(b, &current) == nil && current.Version == graftQueueVersion {
		// current format
		state = current
	} else {
		// Read the map format of 558155c~254ab52 once and promote it to an ordered CAS event (seq=0). Silently discard corrupted JSON and fail-closed.
		legacy := map[string][]string{}
		if err := json.Unmarshal(b, &legacy); err != nil {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue: %w", err)
		}
		state = graftQueueState{Version: graftQueueVersion}
		ids := make([]string, 0, len(legacy))
		for id := range legacy {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			state.Events = append(state.Events, domain.GraftQueueEvent{Snapshot: id, Parents: legacy[id], Legacy: true})
		}
	}
	for _, event := range state.Events {
		if err := domain.ValidateContentHash(domain.ContentHash(event.Snapshot)); err != nil {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue snapshot: %w", err)
		}
		if len(event.Parents) == 0 || len(event.Parents) > 16 {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue parents")
		}
		if event.ExpectedSeq > domain.MaxGraftSeq {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue expected_seq")
		}
		for _, parent := range event.Parents {
			if err := domain.ValidateContentHash(domain.ContentHash(parent)); err != nil {
				return graftQueueState{}, fmt.Errorf("corrupted graft queue parent: %w", err)
			}
		}
	}
	return state, nil
}

func writeGraftQueue(repoRoot, rel string, state graftQueueState) error {
	if len(state.Events) == 0 {
		return providerfs.RemoveRepoFile(repoRoot, rel)
	}
	state.Version = graftQueueVersion
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(repoRoot, rel, b, 0o644)
}

func readPromotions(root string) (map[domain.ContentHash]string, error) {
	m := map[domain.ContentHash]string{}
	b, err := providerfs.ReadRepoFile(root, ".cxt/promotions.json")
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("corrupted promotion queue: %w", err)
	}
	if m == nil {
		m = map[domain.ContentHash]string{}
	}
	return m, nil
}
func writePromotions(root string, m map[domain.ContentHash]string) error {
	if len(m) == 0 {
		return providerfs.RemoveRepoFile(root, ".cxt/promotions.json")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(root, ".cxt/promotions.json", b, 0644)
}
func (s *SyncOutboxAdapter) EnqueuePromotion(ctx context.Context, root string, id domain.ContentHash, message string) error {
	return s.withLock(ctx, root, "promotions", func() error {
		m, err := readPromotions(root)
		if err != nil {
			return err
		}
		m[id] = message
		return writePromotions(root, m)
	})
}
func (s *SyncOutboxAdapter) ListPromotions(ctx context.Context, root string) (m map[domain.ContentHash]string, err error) {
	err = s.withLock(ctx, root, "promotions", func() error { var e error; m, e = readPromotions(root); return e })
	return
}
func (s *SyncOutboxAdapter) AcknowledgePromotion(ctx context.Context, root string, id domain.ContentHash, message string) error {
	return s.withLock(ctx, root, "promotions", func() error {
		m, err := readPromotions(root)
		if err != nil {
			return err
		}
		if current, ok := m[id]; !ok || current != message {
			return nil
		}
		delete(m, id)
		return writePromotions(root, m)
	})
}

var _ outbound.SyncOutbox = (*SyncOutboxAdapter)(nil)
