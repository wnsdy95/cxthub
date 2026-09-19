package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Integration fixtures explicitly assemble real local adapters. Production
// constructors require these ports and have no hidden filesystem fallback.
func newTestSaveService(g outbound.GitContext, c map[domain.ProviderKind]outbound.CaptureSource, d map[domain.ProviderKind]outbound.ProviderCodec, s outbound.SessionStore) *SaveSessionService {
	return NewSaveSessionService(g, c, d, s, capture.NewSessionCapture(s), storage.NewSyncOutbox())
}
func newTestStashService(g outbound.GitContext, c map[domain.ProviderKind]outbound.CaptureSource, d map[domain.ProviderKind]outbound.ProviderCodec, s outbound.SessionStore, l inbound.LoadSession) *StashService {
	return NewStashService(g, c, d, s, l, capture.NewSessionCapture(s))
}
func newTestSyncService(s outbound.SessionStore, r outbound.RemoteSync, g outbound.GitContext) *SyncRepoService {
	return NewSyncRepoService(s, r, g, storage.NewSyncOutbox())
}

type graftQueueEvent = domain.GraftQueueEvent
type graftQueueState struct {
	Version int               `json:"version"`
	Events  []graftQueueEvent `json:"events"`
}

const graftQueueVersion = 1

func readGraftQueue(root, _ string) (state graftQueueState, err error) {
	state.Version = 1
	err = storage.NewSyncOutbox().WithGrafts(context.Background(), root, func(q outbound.GraftQueueAccess) error { var e error; state.Events, e = q.Load(); return e })
	return
}
func writeGraftQueue(root, _ string, state graftQueueState) error {
	return storage.NewSyncOutbox().WithGrafts(context.Background(), root, func(q outbound.GraftQueueAccess) error { return q.Store(state.Events) })
}
func queueGraft(root string, head, parent domain.ContentHash, seq uint64) error {
	return storage.NewSyncOutbox().WithGrafts(context.Background(), root, func(q outbound.GraftQueueAccess) error {
		events, err := q.Load()
		if err != nil {
			return err
		}
		if hasLegacyGraftEvent(events, string(head)) {
			return fmt.Errorf("legacy graft queue remains; flush first")
		}
		if !appendGraftQueueEvent(&events, domain.GraftQueueEvent{Snapshot: string(head), Parents: []string{string(parent)}, ExpectedSeq: seq}) {
			return nil
		}
		return q.Store(events)
	})
}
