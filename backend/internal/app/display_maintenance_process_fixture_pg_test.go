//go:build postgres

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

type displayMaintenanceProcessFixture struct {
	dsn        string
	migrations string
	target     domain.DocDisplayTarget
	source     domain.ContentHash
	canonical  []byte
	event      []byte
	orphans    []string
}

type displayMaintenanceProcessPublication struct {
	Generation domain.DocDisplayProjectionGeneration
	Pages      []domain.DocDisplayPage
	Find       domain.DocFindPage
	Canonical  []byte
}

func isolatedDisplayMaintenanceProcessDSN(test *testing.T, ctx context.Context) string {
	test.Helper()
	dsn := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset; collector process fixture not constructed")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("invalid collector fixture CXT_TEST_DSN")
	}
	localHost := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !localHost(config.ConnConfig.Host) {
		test.Fatal("collector fixture requires a local CXT_TEST_DSN")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !localHost(fallback.Host) {
			test.Fatal("collector fixture requires local CXT_TEST_DSN fallbacks")
		}
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	database := domain.NewID("collector_process_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		test.Fatal("create owned collector fixture database", err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP DATABASE "+quoted)
		admin.Close()
		if err != nil {
			test.Error("drop owned collector fixture after all child pools close", err)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal(err)
		}
		parsed.Path = "/" + database
		query := parsed.Query()
		query.Set("dbname", database)
		query.Set("pool_max_conns", "2")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " dbname=" + database + " pool_max_conns=2"
	}
	configuration, err := pgxpool.ParseConfig(dsn)
	if err != nil || configuration.ConnConfig.Database != database {
		test.Fatal("collector DSN did not select exactly its fresh owned database", err)
	}
	return dsn
}

func openDisplayMaintenanceProcessFixture(test *testing.T, ctx context.Context, fixture displayMaintenanceProcessFixture) (*serverruntime.Infrastructure, *store.PostgresStore) {
	test.Helper()
	infrastructure, err := serverruntime.OpenInfrastructure(ctx, serverruntime.InfrastructureConfig{
		DSN: fixture.dsn, RequirePostgres: true, MigrationsDirectory: fixture.migrations,
		LegacyByteRangePublication: true,
	})
	if err != nil {
		test.Fatal("parent fixture infrastructure", err)
	}
	storage, supported := infrastructure.Store.(*store.PostgresStore)
	if !supported {
		_ = infrastructure.Close()
		test.Fatal("collector fixture requires the actual PostgreSQL adapter")
	}
	return infrastructure, storage
}

func seedDisplayMaintenanceProcessPG(test *testing.T, ctx context.Context, dsn string) displayMaintenanceProcessFixture {
	test.Helper()
	migrations, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		test.Fatal(err)
	}
	seed, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal("open owned collector fixture seed store", err)
	}
	_, err = seed.ApplyPublicMigrations(ctx, migrations)
	seed.Close()
	if err != nil {
		test.Fatal("migrate only the owned collector fixture database", err)
	}
	fixture := displayMaintenanceProcessFixture{dsn: dsn, migrations: migrations}
	infrastructure, storage := openDisplayMaintenanceProcessFixture(test, ctx, fixture)
	defer func() {
		if err := infrastructure.Close(); err != nil {
			test.Error("parent seed close", err)
		}
	}()
	ctx = inbound.WithSystemActor(ctx)
	service := app.NewService(storage, storage, nil, nil, storage)
	service.ConfigureDocumentDisplay(storage)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventToolCall, ToolName: "generic", CallID: "collector-process",
			Input: map[string]any{"original": "원문🙂 marker", "nested": []any{true, nil, "value"}}}},
	}}
	fixture.canonical, err = domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(fixture.canonical)
	if _, err := storage.PutDoc(ctx, repo, document); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocRangeJob(ctx, repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := service.ProcessDocRangeJobs(ctx, 1); err != nil {
		test.Fatal("parent range preparation", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		generation, err := storage.OpenDocRangeGeneration(read, repo, document.Hash)
		if err != nil {
			return err
		}
		entries, err := generation.ReadPage(read, 0)
		if err != nil {
			return err
		}
		if len(entries) != 1 || generation.Descriptor().Identity() != domain.DocumentIdentityLegacy {
			return domain.ErrIntegrity
		}
		fixture.target = domain.DocDisplayTarget{RepoID: repo, DocHash: document.Hash, Binding: generation.Binding(),
			Event: entries[0].DocEventIndex, Profile: domain.DocDisplayProjectionProfile}
		return fixture.target.Validate()
	}); err != nil {
		test.Fatal("actual legacy range target", err)
	}
	entry := fixture.target.Event
	var original struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(fixture.canonical, &original); err != nil || len(original.Events) != 1 {
		test.Fatal("original canonical event stream", err)
	}
	stream := []byte(original.Events[0])
	if entry.Offset < 0 || entry.Length <= 0 || entry.Offset > len(stream)-entry.Length {
		test.Fatal("range target escaped original event stream bytes", entry)
	}
	fixture.event = append([]byte(nil), stream[entry.Offset:entry.Offset+entry.Length]...)
	if domain.HashContent(fixture.event) != entry.Hash {
		test.Fatal("legacy target does not name the exact original event bytes")
	}
	if _, err := storage.EnqueueDocDisplay(ctx, fixture.target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	completed := false
	for attempt := 0; attempt < 64; attempt++ {
		if err := service.ProcessDocDisplayJobs(ctx, 1); err != nil {
			test.Fatal("parent display preparation", attempt, err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state,coalesce(source_hash,'') FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`, repo, document.Hash, entry.Index, fixture.target.Profile).Scan(&state, &fixture.source); err != nil {
			test.Fatal(err)
		}
		if state == "completed" {
			completed = true
			break
		}
		if state != "waiting" {
			test.Fatal("small display fixture did not yield resumable work", state)
		}
	}
	if !completed || domain.ValidateContentHash(fixture.source) != nil {
		test.Fatal("parent did not complete the actual display source")
	}
	assertDisplayMaintenanceProcessCatalogsPG(test, ctx, pool, true)
	for ordinal := 0; ordinal <= domain.DocDisplayMaintenanceBatch; ordinal++ {
		raw := []byte(fmt.Sprintf(`{"collector_process_orphan":%d}`, ordinal))
		hash := domain.HashContent(raw)
		if _, err := pool.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.source, hash, raw); err != nil {
			test.Fatal("original-format object insertion with all SQL guards enabled", err)
		}
		fixture.orphans = append(fixture.orphans, string(hash))
	}
	return fixture
}

func assertDisplayMaintenanceProcessCatalogsPG(test *testing.T, ctx context.Context, pool *pgxpool.Pool, populated bool) {
	test.Helper()
	for _, table := range []string{"doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_key_sources_v1",
		"doc_tool_key_observations_v1", "doc_tool_key_members_v1", "doc_tool_order_sources_v1", "doc_tool_order_plans_v1"} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
			test.Fatal("private catalog count", table, err)
		}
		if populated && count == 0 || !populated && count != 0 {
			test.Fatal("unexpected actual private catalog lifetime", table, count, populated)
		}
	}
}

func displayMaintenanceProcessObjectsPG(test *testing.T, ctx context.Context, pool *pgxpool.Pool, source domain.ContentHash) (int, int64) {
	test.Helper()
	var count int
	var size int64
	if err := pool.QueryRow(ctx, `SELECT count(*),coalesce(sum(octet_length(bytes)),0)
 FROM doc_display_objects_v1 WHERE source_hash=$1`, source).Scan(&count, &size); err != nil {
		test.Fatal(err)
	}
	return count, size
}

func displayMaintenanceProcessOrphansPG(test *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture displayMaintenanceProcessFixture) int {
	test.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=ANY($2::text[])`, fixture.source, fixture.orphans).Scan(&count); err != nil {
		test.Fatal(err)
	}
	return count
}

func readDisplayMaintenanceProcessPG(test *testing.T, ctx context.Context, fixture displayMaintenanceProcessFixture) displayMaintenanceProcessPublication {
	test.Helper()
	infrastructure, storage := openDisplayMaintenanceProcessFixture(test, ctx, fixture)
	defer func() {
		if err := infrastructure.Close(); err != nil {
			test.Error("parent published reader close", err)
		}
	}()
	ctx = inbound.WithSystemActor(ctx)
	var publication displayMaintenanceProcessPublication
	oracle, err := domain.BuildDocEventDisplay(ctx, fixture.event)
	if err != nil || oracle == nil || !strings.Contains(oracle.Text(), "원문🙂 marker") {
		test.Fatal("small original event display oracle", err)
	}
	if err := storage.WithinReadSnapshot(outbound.WithDocReadOnly(ctx), func(read context.Context) error {
		var err error
		publication.Generation, err = storage.ReadDocEventDisplayGeneration(read, fixture.target)
		if err != nil {
			return err
		}
		var text strings.Builder
		for position := 0; position < publication.Generation.Pages.Count; position++ {
			page, err := storage.ReadDocEventDisplayPage(read, fixture.target, position)
			if err != nil {
				return err
			}
			if page.From != text.Len() {
				return domain.ErrIntegrity
			}
			for offset, character := range page.Text {
				width := utf8.RuneLen(character)
				actual, err := page.SourceRange(offset, width)
				if err != nil {
					return err
				}
				expected, err := oracle.SourceRange(page.From+offset, width)
				if err != nil || actual != expected {
					return domain.ErrIntegrity
				}
			}
			text.WriteString(page.Text)
			publication.Pages = append(publication.Pages, page)
		}
		if len(publication.Pages) == 0 || text.String() != oracle.Text() || text.Len() != publication.Generation.Source.TextBytes {
			return domain.ErrIntegrity
		}
		return nil
	}); err != nil {
		test.Fatal("published readonly pages must keep exact original text and rune spans", err)
	}
	service := app.NewService(storage, storage, nil, nil, storage)
	service.ConfigureDocumentDisplay(storage)
	request := domain.DocFindPageRequest{Legacy: true, Query: "원문🙂", Total: 1, To: 1, Representation: domain.DocFindDisplayRepresentation}
	publication.Find, err = service.FindDocPage(outbound.WithDocReadOnly(ctx), fixture.target.RepoID, fixture.target.DocHash, request)
	if err != nil || len(publication.Find.Matches) != 1 {
		test.Fatal("public readonly display read", publication.Find, err)
	}
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrForbidden
	})
	if page, err := service.FindDocPage(outbound.WithDocReadOnly(denied), fixture.target.RepoID, fixture.target.DocHash, request); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("published display bypassed fresh read authority", page, err)
	}
	document, err := storage.GetDoc(ctx, fixture.target.RepoID, fixture.target.DocHash)
	if err != nil || document.DocumentRef() != (domain.DocumentRef{Hash: fixture.target.DocHash, Identity: domain.DocumentIdentityLegacy}) {
		test.Fatal("collector changed original document identity", document.DocumentRef(), err)
	}
	publication.Canonical, err = domain.CanonicalBytes(document.CIR)
	if err != nil || !bytes.Equal(publication.Canonical, fixture.canonical) {
		test.Fatal("collector changed exact original archive bytes", err)
	}
	return publication
}
