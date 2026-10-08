package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"testing"
)

type p6TxKey struct{}
type p6Store struct {
	outbound.MetadataStore
	outbound.RepositoryInitializationStore
	readError   error
	root        bool
	optInAtLock bool
	effects     int
	revisions   int
	checks      int
}

func (s *p6Store) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	if ctx.Value(p6TxKey{}) != true {
		return domain.Repo{}, errors.New("outside transaction")
	}
	s.checks++
	if s.readError != nil {
		return domain.Repo{}, s.readError
	}
	v := ""
	if s.root {
		v = "cxt-manifest-sha256-v1"
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "required_doc_identity": v})
	var r domain.Repo
	err := json.Unmarshal(raw, &r)
	return r, err
}
func (s *p6Store) WithinRepository(ctx context.Context, id domain.ContentHash, fn func(context.Context) error) error {
	if s.optInAtLock {
		s.root = true
	}
	return fn(context.WithValue(ctx, p6TxKey{}, true))
}
func (s *p6Store) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, p6TxKey{}, true))
}
func (s *p6Store) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	return domain.RepositoryRevision{}, nil
}
func (s *p6Store) AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error {
	s.revisions++
	return nil
}
func TestP6OldPeerConcurrentOptInBeforeEffects(t *testing.T) {
	st := &p6Store{optInAtLock: true}
	svc := NewService(st, nil, nil, nil, nil)
	err := repositoryWriteError(inbound.WithSystemActor(context.Background()), svc, domain.HashContent([]byte("p6")), func(context.Context) error { st.effects++; return nil })
	if err == nil || !strings.Contains(err.Error(), "document identity upgrade required") {
		t.Fatalf("old/system peer not fenced: %v", err)
	}
	if st.effects != 0 || st.revisions != 0 || st.checks == 0 {
		t.Fatalf("effects=%d revisions=%d checks=%d", st.effects, st.revisions, st.checks)
	}
}
func TestP6LegacyWriteStillWorks(t *testing.T) {
	st := &p6Store{}
	svc := NewService(st, nil, nil, nil, nil)
	err := repositoryWriteError(inbound.WithSystemActor(context.Background()), svc, domain.HashContent([]byte("p6")), func(context.Context) error { st.effects++; return nil })
	if err != nil || st.effects != 1 || st.revisions != 1 {
		t.Fatalf("legacy failed: %v", err)
	}
}
func TestP6IncompleteAdapterCannotEnable(t *testing.T) {
	svc := NewService(&p6Store{}, nil, nil, nil, nil)
	cap, ok := any(svc).(interface {
		ConfigureConversationRootPublication(bool) error
		RootPublicationEnabled() bool
		DocumentIdentitiesSupported() []domain.DocumentIdentity
	})
	if !ok {
		t.Fatal("missing capability API")
	}
	if cap.ConfigureConversationRootPublication(false) != nil || cap.RootPublicationEnabled() {
		t.Fatal("default enabled")
	}
	ids := cap.DocumentIdentitiesSupported()
	if len(ids) != 1 || ids[0] != domain.DocumentIdentityLegacy {
		t.Fatal("overadvertises")
	}
	if cap.ConfigureConversationRootPublication(true) == nil || cap.RootPublicationEnabled() {
		t.Fatal("incomplete adapter enabled root admission")
	}
}

func (s *p6Store) RequireDocumentIdentity(context.Context, domain.ContentHash, domain.DocumentIdentity) error {
	s.effects++
	return nil
}
func (s *p6Store) UpdateRepoAbout(context.Context, domain.ContentHash, string, string, []string) error {
	s.effects++
	return nil
}
func (s *p6Store) UpdateRepoConfig(context.Context, domain.ContentHash, *string, *bool) error {
	s.effects++
	return nil
}
func (s *p6Store) GetRepositoryInitialization(context.Context, domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	s.effects++
	return domain.RepositoryInitializationReceipt{}, domain.ErrNotFound
}
func TestP6PinnedReadRejectsBeforePayload(t *testing.T) {
	st := &p6Store{root: true}
	svc := NewService(st, nil, nil, nil, nil)
	_, err := repositoryReadForRepo(context.Background(), svc, domain.HashContent([]byte("read")), func(context.Context) (int, error) { st.effects++; return 1, nil })
	if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || st.effects != 0 || st.checks != 1 {
		t.Fatalf("unpinned read or leaked payload: %v %+v", err, st)
	}
}
func TestP6ProfileDefaultOffBeforeAnyEffect(t *testing.T) {
	st := &p6Store{}
	svc := NewService(st, nil, nil, nil, nil)
	root := domain.DocumentIdentityRootV1
	desc := "must not change"
	branch := "must-not-change"
	ctx := inbound.WithDocumentIdentities(inbound.WithSystemActor(context.Background()), []domain.DocumentIdentity{root})
	_, err := svc.PatchRepoProfile(ctx, domain.HashContent([]byte("profile")), inbound.RepoProfilePatch{RequiredDocIdentity: &root, Description: &desc, DefaultBranch: &branch})
	if !errors.Is(err, domain.ErrRootPublicationDisabled) || st.effects != 0 || st.revisions != 0 {
		t.Fatalf("default-off effects: %v %+v", err, st)
	}
}
func TestP6PeerDeclarationCannotAuthorizeWrite(t *testing.T) {
	st := &p6Store{}
	svc := NewService(st, nil, nil, nil, nil)
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	err := repositoryWriteError(ctx, svc, domain.HashContent([]byte("auth")), func(context.Context) error { st.effects++; return nil })
	if !errors.Is(err, domain.ErrUnauthorized) || st.effects != 0 || st.revisions != 0 {
		t.Fatalf("declaration granted authority: %v %+v", err, st)
	}
}
func TestP6WorkerContextIsBinaryBoundAndKeepsActor(t *testing.T) {
	st := &p6Store{root: true}
	svc := NewService(st, nil, nil, nil, nil)
	ctx := svc.DocumentIdentityWorkerContext(inbound.WithSystemActor(context.Background()))
	if _, system := inbound.RepositoryActor(ctx); !system {
		t.Fatal("actor lost")
	}
	if ids := inbound.DocumentIdentities(ctx); len(ids) != 1 || ids[0] != domain.DocumentIdentityLegacy {
		t.Fatal("worker overclaims incomplete adapter support", ids)
	}
	err := repositoryWriteError(ctx, svc, domain.HashContent([]byte("worker")), func(context.Context) error { st.effects++; return nil })
	if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || st.effects != 0 {
		t.Fatal("worker bypass", err)
	}
}
func TestP6DiscoveryPinsRequirementBeforeStateRead(t *testing.T) {
	st := &p6Store{root: true}
	svc := NewService(st, nil, nil, nil, nil)
	repo, pending, err := svc.GetRepositoryInitializationView(context.Background(), domain.HashContent([]byte("discovery")), "main")
	if err != nil || pending || repo.RequiredDocIdentity != domain.DocumentIdentityRootV1 || st.effects != 0 || st.checks != 1 {
		t.Fatalf("discovery leaked state: %+v %v %v %+v", repo, pending, err, st)
	}
}

func TestP6DirectReadsRejectBeforeStorePayload(t *testing.T) {
	st := &p6Store{root: true}
	s := NewService(st, nil, nil, nil, nil)
	ctx := context.Background()
	repo := domain.HashContent([]byte("direct"))
	hash := domain.HashContent([]byte("doc"))
	tests := map[string]func() error{
		"negotiate":              func() error { _, e := s.Negotiate(ctx, inbound.PushNegotiateInput{RepoID: repo}); return e },
		"chunks":                 func() error { _, e := s.PullChunks(ctx, inbound.PullChunksInput{RepoID: repo}); return e },
		"diff":                   func() error { _, e := s.Diff(ctx, inbound.DiffInput{RepoID: repo, Left: hash, Right: hash}); return e },
		"search":                 func() error { _, e := s.Search(ctx, inbound.SearchInput{RepoID: repo, Query: "ok"}); return e },
		"history":                func() error { _, e := s.ListHistory(ctx, repo); return e },
		"reflog":                 func() error { _, e := s.Reflog(ctx, repo); return e },
		"snapshot":               func() error { _, e := s.GetSnapshot(ctx, repo, hash); return e },
		"memory":                 func() error { _, e := s.GetMemoryObject(ctx, repo, hash); return e },
		"pending":                func() error { _, e := s.ListPendings(ctx, repo); return e },
		"unsync":                 func() error { _, e := s.ListUnsyncs(ctx, repo); return e },
		"secrets":                func() error { _, e := s.GetSecrets(ctx, repo); return e },
		"settings":               func() error { _, e := s.GetSettings(ctx, repo, "claude"); return e },
		"settings-object":        func() error { _, e := s.GetSettingsObjectByHash(ctx, repo, hash); return e },
		"revision":               func() error { _, e := s.RepositoryRevision(ctx, repo); return e },
		"promotions":             func() error { _, e := s.ListPRPromotions(ctx, repo); return e },
		"events":                 func() error { _, e := s.ReadDocEvents(ctx, repo, hash, "", 0, 1); return e },
		"event-search":           func() error { _, e := s.SearchDocEvents(ctx, repo, hash, "ok", 0, 1); return e },
		"matching-docs":          func() error { _, e := s.MatchingDocHashes(ctx, repo, "ok"); return e },
		"fragments":              func() error { _, e := s.ReadDocFragments(ctx, repo, hash, 0, 0, 1, 1024); return e },
		"view":                   func() error { _, e := s.GetRepositoryView(ctx, repo); return e },
		"pending-view":           func() error { _, e := s.GetPendingView(ctx, repo); return e },
		"refs":                   func() error { _, e := s.ListRefs(ctx, repo); return e },
		"doc":                    func() error { _, e := s.GetDoc(ctx, repo, hash); return e },
		"manifest":               func() error { _, e := s.GetManifest(ctx, repo); return e },
		"list":                   func() error { _, e := s.List(ctx, inbound.ListSnapshotsInput{RepoID: repo}); return e },
		"fsck":                   func() error { _, e := s.Fsck(ctx, repo); return e },
		"initialization-receipt": func() error { _, e := s.GetRepositoryInitialization(inbound.WithSystemActor(ctx), repo); return e },
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal("unfenced read", err)
			}
		})
	}
	if st.effects != 0 {
		t.Fatal("state touched", st.effects)
	}
}

func TestP6MissingLegacyMetadataAndStorageFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readError  error
		wantEffect bool
	}{{"unregistered-legacy", domain.ErrNotFound, true}, {"storage-error", errors.New("read failed"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			st := &p6Store{readError: tc.readError}
			s := NewService(st, nil, nil, nil, nil)
			_, err := repositoryReadForRepo(context.Background(), s, domain.HashContent([]byte(tc.name)), func(context.Context) (int, error) { st.effects++; return 3, nil })
			if (err == nil) != tc.wantEffect || (st.effects == 1) != tc.wantEffect {
				t.Fatal(err, st.effects)
			}
		})
	}
}
