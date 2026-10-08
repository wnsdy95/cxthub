//go:build postgres

package app

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type a11UnavailableChunk struct{ *store.PostgresStore }

var a11TemporaryRead = errors.New("synthetic temporary root read failure")

func (a11UnavailableChunk) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, a11TemporaryRead
}

type a11BeforeCompletion struct {
	*store.PostgresStore
	before func() error
}

type a11PreparedCompletion struct {
	outbound.PreparedDocPublication
	before func() error
}

func (s a11BeforeCompletion) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	plan, err := s.PostgresStore.PrepareDocJob(ctx, doc)
	return a11PreparedCompletion{plan, s.before}, err
}

func (p a11PreparedCompletion) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	if err := p.before(); err != nil {
		return err
	}
	return p.PreparedDocPublication.Complete(ctx, job, now)
}

func a11RootRepresentation(t *testing.T, text string) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	unique := t.Name() + text + time.Now().UTC().String()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: unique, SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: unique}}}}}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, bodies
}

// Each case exercises the real global claim against only its own durable queue.
// The configured database may contain other suites' pending jobs; never process
// or delete them. Match the existing catalog-upgrade disposable database pattern.
func a11IsolatedPGDSN(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := collaborationDSN(t)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	database := domain.NewID("root_startup_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP DATABASE "+quoted)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + database
		q := u.Query()
		q.Set("dbname", database)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " dbname=" + database
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != database {
		t.Fatal("isolated DSN did not select the owned database", err)
	}
	t.Logf("isolated global queue in owned database %s", database)
	return dsn
}

func TestRootStartupPGAcceptedJobRestartWithAdmissionOff(t *testing.T) {
	if !conversationRootReleaseReady {
		t.Skip("requires released binary or explicitly authorized private Go readiness overlay")
	}
	for _, mode := range []string{"waiting", "retrying", "corrupt-current", "revoked-current", "late-revoke"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
			t.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(t, ctx))
			svc, st, repo := collaborationPG(t)
			fixture := makeTeamFixture(t, st)
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
				t.Fatal(err)
			}
			if err := svc.ConfigureConversationRootPublication(true); err != nil {
				t.Fatal("complete PG adapter failed ready startup", err)
			}
			root := domain.DocumentIdentityRootV1
			if _, err := svc.PatchRepoProfile(ctx, repo, inbound.RepoProfilePatch{RequiredDocIdentity: &root}); err != nil {
				t.Fatal(err)
			}
			if mode == "waiting" {
				// Identity-only authorization still uses actual binary support,
				// not a zero content Service with no injected adapters.
				archived := false
				identity := NewIdentityService(nil, st)
				if _, err := identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, RepositoryPatch{Archived: &archived}); err != nil {
					t.Fatal("complete binary rejected authorized identity mutation", err)
				}
				if _, err := identity.UpdateRepositorySettings(context.Background(), fixture.owner.ID, fixture.repository.ID, RepositoryPatch{Archived: &archived}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
					t.Fatal("identity mutation admitted old peer", err)
				}
			}
			representation, bodies := a11RootRepresentation(t, "accepted")
			chunks := make([]inbound.ChunkObject, 0, len(bodies))
			for hash, data := range bodies {
				chunks = append(chunks, inbound.ChunkObject{Hash: hash, Data: data})
			}
			if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks}); err != nil {
				t.Fatal(err)
			}
			accepted, err := svc.SubmitDocFinalization(ctx, repo, representation)
			if err != nil || accepted.State != "waiting" || accepted.DocIdentity != root {
				t.Fatal("public admission did not create waiting root", accepted, err)
			}
			var stale domain.DocFinalizationJob
			if mode == "retrying" {
				// A real failed worker persists retry state before its process exits.
				svc.blobs = a11UnavailableChunk{st}
				if err := svc.ProcessDocFinalizations(ctx, 1); !errors.Is(err, a11TemporaryRead) {
					t.Fatal("temporary loader failure not preserved", err)
				}
			}
			pending, err := st.GetDocJob(ctx, repo, accepted.ID)
			wantState := "waiting"
			if mode == "retrying" {
				wantState = "retrying"
				stale = pending
			}
			if err != nil || pending.State != wantState || (mode == "retrying" && (pending.Attempts != 1 || pending.Reason != "temporary_failure")) {
				t.Fatal("wrong durable pre-restart state", pending, err)
			}
			st.Close()
			reopened, err := store.NewPostgresStore(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			worker := NewService(reopened, reopened, nil, nil, reopened)
			if worker.RootPublicationEnabled() || worker.ConfigureConversationRootPublication(false) != nil || !hasDocumentIdentity(worker.DocumentIdentitiesSupported(), root) {
				t.Fatal("restarted complete reader lost support or enabled admission")
			}
			loaded, err := reopened.GetDocJob(ctx, repo, accepted.ID)
			if err != nil || !reflect.DeepEqual(loaded, pending) {
				t.Fatal("durable job changed across new pool/service", loaded, err)
			}
			pool, err := pgxpool.New(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			newRoot, _ := a11RootRepresentation(t, "must remain blocked")
			newJob, err := domain.NewDocFinalizationJobForRepresentation(repo, newRoot, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			assertAdmissionOff := func() {
				t.Helper()
				var before, after int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1`, repo).Scan(&before); err != nil {
					t.Fatal(err)
				}
				if _, err := worker.SubmitDocFinalization(ctx, repo, newRoot); !errors.Is(err, domain.ErrRootPublicationDisabled) {
					t.Fatal("new root admitted after restart", err)
				}
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM doc_finalization_jobs WHERE repo_id=$1`, repo).Scan(&after); err != nil || before != after {
					t.Fatal("rejected admission changed jobs", before, after, err)
				}
				if _, err := reopened.GetDocJob(ctx, repo, newJob.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("rejected admission left a job", err)
				}
			}
			assertAdmissionOff()
			legacyRepo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			if _, err := reopened.PutRepo(ctx, domain.Repo{ID: legacyRepo}); err != nil {
				t.Fatal(err)
			}
			if _, err := worker.PatchRepoProfile(ctx, legacyRepo, inbound.RepoProfilePatch{RequiredDocIdentity: &root}); !errors.Is(err, domain.ErrRootPublicationDisabled) {
				t.Fatal("disabled admission allowed new opt-in", err)
			}
			if r, err := reopened.GetRepo(ctx, legacyRepo); err != nil || r.RequiredDocIdentity != domain.DocumentIdentityLegacy {
				t.Fatal("failed opt-in mutated requirement", r, err)
			}
			manifest, err := representation.ConversationManifest()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "corrupt-current":
				_, err = pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, manifest.Chunks[0].Hash, []byte("invalid compressed bytes after restart"))
			case "revoked-current":
				_, err = pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='chunk'`, repo, manifest.Chunks[0].Hash)
			case "late-revoke":
				// Revoke ownership after current-byte proof but before the final
				// publication transaction; the old proof must not authorize it.
				worker.blobs = a11BeforeCompletion{reopened, func() error {
					_, err := pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='chunk'`, repo, manifest.Chunks[0].Hash)
					return err
				}}
			}
			if err != nil {
				t.Fatal(err)
			}
			if delay := time.Until(pending.NextAttempt); delay > 0 {
				if delay > 3*time.Second {
					t.Fatal("unexpected retry envelope", delay)
				}
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			err = worker.ProcessDocFinalizations(ctx, 1)
			done, statusErr := reopened.GetDocJob(ctx, repo, accepted.ID)
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			if mode == "waiting" || mode == "retrying" {
				if err != nil || done.State != "completed" || done.DocumentRef() != representation.DocumentRef() || done.Version <= pending.Version {
					t.Fatal("accepted root did not recover with admission off", done, err)
				}
				proof, err := reopened.ReadVerifiedDoc(ctx, repo, representation.Hash)
				if err != nil || proof.DocumentRef() != representation.DocumentRef() {
					t.Fatal("recovered receipt lacks current exact proof", err)
				}
				if mode == "retrying" {
					compat := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{root}, worker.DocumentIdentitiesSupported())
					if err := reopened.CompleteDocJob(compat, stale, proof, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
						t.Fatal("stale pre-restart claim completed", err)
					}
					if current, err := reopened.GetDocJob(ctx, repo, accepted.ID); err != nil || !reflect.DeepEqual(current, done) {
						t.Fatal("stale completion changed receipt", current, err)
					}
				}
				// Existing root reads work without enabling new admission.
				snap := domain.Snapshot{ID: representation.Hash, DocHash: representation.Hash, DocIdentity: root, RepoID: repo, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}
				if _, err := worker.Commit(ctx, inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{snap}}); err != nil {
					t.Fatal("prepared metadata publication incorrectly needs new admission", err)
				}
				if doc, err := worker.GetDoc(ctx, repo, snap.ID); err != nil || doc.Hash != snap.ID || doc.Identity != root {
					t.Fatal("existing root read failed with admission off", err)
				}
			} else {
				wantErr, wantState := domain.ErrIntegrity, "rejected"
				if mode == "revoked-current" {
					wantErr, wantState = domain.ErrNotFound, "retrying"
				} else if mode == "late-revoke" {
					wantErr, wantState = domain.ErrNotFound, "retrying"
				}
				if !errors.Is(err, wantErr) || done.State != wantState {
					t.Fatal("restart bypassed current dependency/policy check", done, err)
				}
				if have, err := reopened.HasDocs(ctx, repo, []domain.ContentHash{representation.Hash}); err != nil || len(have) != 0 {
					t.Fatal("failed restart published root", have, err)
				}
			}
			assertAdmissionOff()
			t.Logf("accepted=%s reopened=%s final=%s admission=false", wantState, loaded.State, done.State)
		})
	}
}
