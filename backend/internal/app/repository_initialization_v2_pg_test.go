//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func protectedInitializationFixture(t *testing.T) (*Service, *store.PostgresStore, context.Context, context.Context, domain.RepositoryInitializationRequest, domain.RepositoryInitializationReceipt) {
	t.Helper()
	svc, st, _ := collaborationPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	name := fmt.Sprintf("protected%d", time.Now().UnixNano())
	owner := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	member := domain.User{ID: "dev:" + name + "m", Username: name + "m", Name: "member", Email: name + "m@example.test"}
	for _, u := range []domain.User{owner, member} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: name, Name: "code", Slug: "code", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMember}); err != nil {
		t.Fatal(err)
	}
	request := domain.RepositoryInitializationRequest{RemoteURL: "https://host.test/" + name + "/code", DefaultBranch: "main"}
	id := domain.HashContent([]byte(normalizeGitURL(request.RemoteURL)))
	own, mem := inbound.WithRepositoryActor(ctx, owner.ID), inbound.WithRepositoryActor(ctx, member.ID)
	if _, err := svc.BeginRepositoryInitialization(mem, member.ID, id, request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("member created protected repo", err)
	}
	if _, err := st.GetRepo(ctx, id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rejected begin left repo", err)
	}
	receipt, err := svc.BeginRepositoryInitialization(own, owner.ID, id, request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Repo.ContextProtocol != 1 {
		t.Fatal("receipt not protected")
	}
	return svc, st, own, mem, request, receipt
}

func initializationMutableSnapshot(t *testing.T, st *store.PostgresStore, repo domain.ContentHash) domain.ContentHash {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + "synthetic mutable checkpoint"}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(context.Background(), repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(context.Background(), domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Branch: domain.StashBranchLabel, Message: domain.HookMessagePrefix + "fixture"}); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

func TestPGRepositoryInitializationModernFirstMember(t *testing.T) {
	for _, emptyBirth := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_birth_%v", emptyBirth), func(t *testing.T) {
			svc, st, owner, member, request, receipt := protectedInitializationFixture(t)
			id := receipt.Repo.ID
			actor, _ := inbound.RepositoryActor(member)
			if _, err := svc.EnsureRepo(member, actor, receipt.Repo); err != nil {
				t.Fatal("ordinary member registration", err)
			}
			a := collaborationSnapshot(t, st, id, "local main checkpoint")
			b := collaborationSnapshot(t, st, id, "feature first commit", a)
			// Legacy clients cannot exploit a protected-but-empty repository.
			old := domain.Ref{RepoID: id, Kind: domain.RefBranch, Name: "main", Target: a}
			if _, err := svc.UpdateRef(member, inbound.UpdateRefInput{RepoID: id, Ref: old}); err == nil {
				t.Fatal("old client anonymous ref accepted")
			}
			old.BranchID = domain.LegacyContextBranchID(string(id), "main")
			if err := st.CompareAndSwapRef(owner, id, old, ""); err == nil {
				t.Fatal("generic CAS legacy exception leaked")
			}
			birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(id), Kind: "birth", Branch: "feature", BranchID: "actual-feature-birth", Source: a, Target: a, GitAfter: strings.Repeat("1", 40), CreatedAt: time.Now().UTC(), Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD", StartCommit: strings.Repeat("1", 40), OriginBranch: "main", OriginBranchID: domain.LegacyContextBranchID(string(id), "main")}}
			if emptyBirth {
				birth.Source = ""
				birth.Target = ""
			}
			if err := svc.RecordHistory(member, birth); err != nil {
				t.Fatal("member ordinary genuine birth", err)
			}
			_, available, err := svc.GetRepositoryInitializationView(owner, id, "main")
			if err != nil || !available {
				t.Fatal("independent birth blocked main", err)
			}
			ownID, _ := inbound.RepositoryActor(owner)
			recovered, err := svc.BeginRepositoryInitialization(owner, ownID, id, request)
			if err != nil || !reflect.DeepEqual(recovered, receipt) {
				t.Fatal("lost creation ack after birth", err)
			}
			expected := a
			if emptyBirth {
				expected = ""
			}
			ref := domain.Ref{RepoID: id, Kind: domain.RefBranch, Name: "feature", BranchID: birth.BranchID, Target: b}
			if _, err := svc.UpdateRef(member, inbound.UpdateRefInput{RepoID: id, Ref: ref, ExpectedTarget: expected}); err != nil {
				t.Fatal("ordinary member initial ref", err)
			}
			if err := svc.RecordHistory(member, birth); err != nil {
				t.Fatal("birth retry", err)
			}
			got, _ := st.GetRef(owner, id, domain.RefBranch, "feature")
			if got.Target != b {
				t.Fatal("birth replay rewound tip")
			}
			if _, err := st.GetRef(owner, id, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("unselected main published", err)
			}
			archive := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: string(id), BranchID: birth.BranchID, Branch: "feature", Kind: "archive", BindingParent: birth.ID, Source: b, Target: b, CreatedAt: time.Now().UTC()}
			if err := svc.RecordHistory(member, archive); err != nil {
				t.Fatal(err)
			}
			_, available, err = svc.GetRepositoryInitializationView(owner, id, "feature")
			if err != nil || available {
				t.Fatal("archive reopened feature anchor", err)
			}
		})
	}
}

// Instrument only this fixture store; all validation still uses real owned PG
// documents. Mutations run after the coherent read captured its exact proof.
type initializationCapturePG struct {
	*store.PostgresStore
	afterCapture func() error
	verifyHook   func(context.Context) error
}

func (s *initializationCapturePG) CaptureRepositoryInitialization(ctx context.Context, id domain.ContentHash, a domain.RepositoryInitializationAnchor, e outbound.RepositoryInitializationEvidence) (outbound.RepositoryInitializationProof, error) {
	p, err := s.PostgresStore.CaptureRepositoryInitialization(ctx, id, a, e)
	if err == nil && s.afterCapture != nil {
		err = s.afterCapture()
	}
	return p, err
}
func (s *initializationCapturePG) VerifyStoredDoc(ctx context.Context, id, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if !s.InReadOnlyTransaction(ctx) {
		return domain.VerifiedDocReference{}, errors.New("body validation under writer lock")
	}
	if s.verifyHook != nil {
		if err := s.verifyHook(ctx); err != nil {
			return domain.VerifiedDocReference{}, err
		}
	}
	return s.PostgresStore.VerifyStoredDoc(ctx, id, hash)
}

func TestPGRepositoryInitializationPreparationRaces(t *testing.T) {
	for _, mode := range []string{"snapshot", "memory_attachment", "revoke", "modern_birth", "exact_other_completion"} {
		t.Run(mode, func(t *testing.T) {
			svc, st, owner, member, _, receipt := protectedInitializationFixture(t)
			id := receipt.Repo.ID
			a := initializationMutableSnapshot(t, st, id)
			snap, _ := st.GetSnapshot(owner, id, a)
			in := initializationFinalize(receipt, snap)
			recorder := &initializationCapturePG{PostgresStore: st}
			testSvc := NewService(recorder, recorder, nil, nil, recorder)
			recorder.afterCapture = func() error {
				switch mode {
				case "snapshot":
					return st.UpdateSnapshotMessage(owner, id, a, "changed")
				case "memory_attachment":
					hash, err := st.PutMemory(owner, id, domain.MemoryDigest{SnapshotID: a, Summary: "concurrent attachment"})
					if err != nil {
						return err
					}
					return st.CompareAndSwapSnapshotMemory(owner, id, a, "", hash)
				case "revoke":
					r, err := st.GetRepository(owner, receipt.Repo.RepositoryID)
					if err != nil {
						return err
					}
					r.Archived = true
					return st.WithinIdentity(owner, func(tx context.Context) error { return st.CreateRepository(tx, r) })
				case "modern_birth":
					return svc.RecordHistory(member, domain.HistoryEvent{ID: strings.Repeat("c", 32), RepoID: string(id), BranchID: "new-main", Branch: "main", Kind: "birth", CreatedAt: time.Now().UTC()})
				case "exact_other_completion":
					if _, err := svc.FinalizeRepositoryInitialization(owner, id, in); err != nil {
						return err
					}
					// A concurrent accepted receipt must win over this redundant read failure.
					return domain.ErrIntegrity
				}
				return nil
			}
			got, err := testSvc.FinalizeRepositoryInitialization(owner, id, in)
			switch mode {
			case "exact_other_completion":
				if err != nil || got.Anchor == nil {
					t.Fatal("exact accepted receipt lost", err)
				}
			case "revoke":
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatal("revoked authority used", err)
				}
			default:
				if !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
					t.Fatal("stale preparation accepted", err)
				}
			}
			if mode != "exact_other_completion" {
				r, _ := initializationReceiptForBranch(owner, st, id, "main")
				if r.Anchor != nil {
					t.Fatal("failure left acceptance")
				}
			}
		})
	}
}

func TestPGRepositoryInitializationBodyVerificationDoesNotBlockWriter(t *testing.T) {
	svc, st, owner, _, _, receipt := protectedInitializationFixture(t)
	id := receipt.Repo.ID
	a := initializationMutableSnapshot(t, st, id)
	snap, _ := st.GetSnapshot(owner, id, a)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	recorder := &initializationCapturePG{PostgresStore: st, verifyHook: func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	testSvc := NewService(recorder, recorder, nil, nil, recorder)
	in := initializationFinalize(receipt, snap)
	go func() { _, err := testSvc.FinalizeRepositoryInitialization(owner, id, in); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("verification did not start", err)
	}
	writer := make(chan error, 1)
	go func() {
		writer <- st.WithinRepository(owner, id, func(tx context.Context) error { return st.UpdateSnapshotMessage(tx, id, a, "concurrent") })
	}()
	var writerErr error
	select {
	case writerErr = <-writer:
	case <-time.After(2 * time.Second):
		close(release)
		<-writer
		<-done
		t.Fatal("body validation blocked graph writer")
	}
	close(release)
	validationErr := <-done
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	if !errors.Is(validationErr, domain.ErrRepositoryInitializationConflict) {
		t.Fatal("stale body proof accepted", validationErr)
	}
	_, available, err := svc.GetRepositoryInitializationView(owner, id, "main")
	if err != nil || !available {
		t.Fatal("failed validation consumed anchor", err)
	}
}

func TestPGRepositoryInitializationExactReplayAfterArchiveAndBodyChange(t *testing.T) {
	svc, st, owner, member, _, receipt := protectedInitializationFixture(t)
	id := receipt.Repo.ID
	a := initializationMutableSnapshot(t, st, id)
	snap, _ := st.GetSnapshot(owner, id, a)
	in := initializationFinalize(receipt, snap)
	if _, err := svc.FinalizeRepositoryInitialization(owner, id, in); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSnapshotMessage(owner, id, a, "metadata changed"); err != nil {
		t.Fatal(err)
	}
	archive := domain.HistoryEvent{ID: strings.Repeat("d", 32), RepoID: string(id), BranchID: in.Anchor.Ref.BranchID, Branch: "main", Kind: "archive", Source: a, Target: a, CreatedAt: time.Now().UTC()}
	if err := svc.RecordHistory(member, archive); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadReflog(owner, id)
	// A verifier that always fails proves accepted retry never visits body validation.
	recorder := &initializationCapturePG{PostgresStore: st, verifyHook: func(context.Context) error { return errors.New("must not reverify receipt") }}
	testSvc := NewService(recorder, recorder, nil, nil, recorder)
	if _, err := testSvc.FinalizeRepositoryInitialization(owner, id, in); err != nil {
		t.Fatal("archive/metadata replay", err)
	}
	after, _ := st.ReadReflog(owner, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("replay appended reflog")
	}
	if _, err := st.GetRef(owner, id, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("replay resurrected branch", err)
	}
}

type initializationViewPausePG struct {
	*store.PostgresStore
	read, resume chan struct{}
}

func (s *initializationViewPausePG) GetRepositoryInitialization(ctx context.Context, id domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	r, err := s.PostgresStore.GetRepositoryInitialization(ctx, id)
	if err == nil {
		close(s.read)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return r, ctx.Err()
		}
	}
	return r, err
}
func TestPGRepositoryInitializationAvailabilityReadCoherent(t *testing.T) {
	svc, st, owner, member, _, receipt := protectedInitializationFixture(t)
	id := receipt.Repo.ID
	paused := &initializationViewPausePG{PostgresStore: st, read: make(chan struct{}), resume: make(chan struct{})}
	reader := NewService(paused, st, nil, nil, st)
	type result struct {
		repo      domain.Repo
		available bool
		err       error
	}
	done := make(chan result, 1)
	go func() { r, a, e := reader.GetRepositoryInitializationView(owner, id, "main"); done <- result{r, a, e} }()
	<-paused.read
	birth := domain.HistoryEvent{ID: strings.Repeat("e", 32), RepoID: string(id), Branch: "main", BranchID: "read-coherence-birth", Kind: "birth", CreatedAt: time.Now().UTC()}
	err := svc.RecordHistory(member, birth)
	close(paused.resume)
	got := <-done
	if err != nil {
		t.Fatal(err)
	}
	if got.err != nil || !got.available || got.repo.ContextProtocol != 1 {
		t.Fatal("mixed read generation", got.err)
	}
	_, available, err := svc.GetRepositoryInitializationView(owner, id, "main")
	if err != nil || available {
		t.Fatal("fresh read ignored history", err)
	}
}
