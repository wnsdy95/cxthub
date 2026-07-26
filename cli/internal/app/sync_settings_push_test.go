package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type settingsPushStore struct {
	outbound.SessionStore
	bundles map[domain.ContentHash]domain.SettingsBundle
}

func (s settingsPushStore) GetSettingsObject(_ context.Context, hash domain.ContentHash) (domain.SettingsBundle, error) {
	bundle, ok := s.bundles[hash]
	if !ok {
		return domain.SettingsBundle{}, domain.ErrNotFound
	}
	return bundle, nil
}

type settingsPushRemote struct {
	outbound.RemoteSync
	hashes []domain.ContentHash
}

func (r *settingsPushRemote) PushSettingsObject(_ context.Context, _ string, hash domain.ContentHash, _ domain.SettingsBundle) error {
	r.hashes = append(r.hashes, hash)
	return nil
}

func TestPushSettingsObjectsCoversAndDeduplicatesSnapshotReferences(t *testing.T) {
	bundle := domain.SettingsBundle{Kind: "claude"}
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		t.Fatal(err)
	}
	store := settingsPushStore{bundles: map[domain.ContentHash]domain.SettingsBundle{hash: bundle}}
	remote := &settingsPushRemote{}
	svc := NewSyncRepoService(store, remote, nil)
	snaps := []domain.Snapshot{{ClaudeSettings: hash}, {ClaudeSettings: hash}}

	if err := svc.pushSettingsObjects(context.Background(), domain.HashContent([]byte("repo")), snaps); err != nil {
		t.Fatal(err)
	}
	if len(remote.hashes) != 1 || remote.hashes[0] != hash {
		t.Fatalf("settings uploads = %v, want one %s", remote.hashes, hash)
	}
}
