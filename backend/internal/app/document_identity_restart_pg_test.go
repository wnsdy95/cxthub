//go:build postgres

package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type a11UnavailableChunk struct{ *store.PostgresStore }

var a11TemporaryRead = errors.New("synthetic temporary root read failure")

func (a11UnavailableChunk) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, a11TemporaryRead
}

func (a11UnavailableChunk) PrepareRootDocJob(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
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

type a11PreparedRootCompletion struct {
	outbound.PreparedRootDocPublication
	before func() error
}

func (s a11BeforeCompletion) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	plan, err := s.PostgresStore.PrepareRootDocJob(ctx, job)
	if err != nil {
		return nil, err
	}
	return a11PreparedRootCompletion{plan, s.before}, nil
}

func (p a11PreparedRootCompletion) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	if err := p.before(); err != nil {
		return err
	}
	return p.PreparedRootDocPublication.Complete(ctx, job, now)
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
