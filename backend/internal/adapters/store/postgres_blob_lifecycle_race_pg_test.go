//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type blobLifecycleDeleteTraceKeyPG struct{}

type blobLifecycleDeleteTracePG struct {
	deleteCount   int
	deletedRows   int64
	outerRetained bool
}

func (trace *blobLifecycleDeleteTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(query.SQL)), " ")
	return context.WithValue(ctx, blobLifecycleDeleteTraceKeyPG{}, statement)
}

func (trace *blobLifecycleDeleteTracePG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryEndData) {
	if query.Err != nil {
		return
	}
	statement, _ := ctx.Value(blobLifecycleDeleteTraceKeyPG{}).(string)
	if strings.HasPrefix(statement, "delete from blobs ") {
		trace.deleteCount++
		trace.deletedRows += query.CommandTag.RowsAffected()
	}
	if strings.HasPrefix(statement, "release savepoint ") {
		trace.outerRetained = connection.PgConn().TxStatus() == 'T'
	}
}

func blobLifecycleDocumentPG(test *testing.T) (domain.SessionDoc, []byte) {
	test.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{
			CIRVersion:      domain.CIRVersionV1,
			SourceProvider:  domain.ProviderCodex,
			SourceModel:     "blob-lifecycle-fixture",
			CapturedAt:      "2026-10-28T00:00:00Z",
			Cwd:             "/synthetic/blob-lifecycle",
			GitBranch:       "main",
			SessionOriginID: domain.NewID("session_"),
			Fidelity:        domain.FidelityFull,
		},
		Events: []domain.CIREvent{},
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	if _, err := domain.VerifySessionDoc(document); err != nil {
		test.Fatal("empty-event document must pass the public document validation", err)
	}
	if _, chunked := domain.PlanDocChunks(canonical); chunked {
		test.Fatal("empty-event document must use the raw representation")
	}
	return document, canonical
}

func assertBlobLifecycleDocumentPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, document domain.SessionDoc, canonical []byte) {
	test.Helper()
	actual, err := storage.GetDoc(ctx, repo, document.Hash)
	if err != nil {
		test.Fatal("existing document grant lost its public reader", err)
	}
	actualBytes, err := domain.CanonicalBytes(actual.CIR)
	if err != nil || actual.Hash != document.Hash || domain.HashContent(actualBytes) != document.Hash || !bytes.Equal(actualBytes, canonical) {
		test.Fatal("existing document changed its exact canonical bytes or hash", err)
	}
	owned, err := storage.HasDocs(ctx, repo, []domain.ContentHash{document.Hash})
	if err != nil || len(owned) != 1 || owned[0] != document.Hash {
		test.Fatal("existing document grant is not current", owned, err)
	}
}

func assertBlobLifecycleUsagePG(test *testing.T, storage *PostgresStore, ctx context.Context, namespace string, hash domain.ContentHash, expectedBytes int64) {
	test.Helper()
	usage := rawRepresentationQuotaUsagePG(test, storage, ctx, namespace)
	if usage.CurrentBytes != expectedBytes {
		test.Fatal("lifecycle changed exact namespace quota accounting", namespace, usage.CurrentBytes, expectedBytes)
	}
	var projectedBytes, sourceBytes, ledgerBytes int64
	var objects, unrelated int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 COALESCE(sum(bytes),0),count(*),count(*) FILTER (WHERE object_key<>$2),
 (SELECT COALESCE(sum(bytes),0) FROM storage_source_objects WHERE namespace_id=$1),
 (SELECT COALESCE(sum(delta_bytes),0) FROM storage_usage_ledger WHERE namespace_id=$1)
 FROM storage_usage_objects WHERE namespace_id=$1`, namespace, "blob:"+string(hash)).
		Scan(&projectedBytes, &objects, &unrelated, &sourceBytes, &ledgerBytes); err != nil {
		test.Fatal("observe exact lifecycle quota projections and ledger", err)
	}
	expectedObjects := 0
	if expectedBytes > 0 {
		expectedObjects = 1
	}
	if projectedBytes != expectedBytes || sourceBytes != expectedBytes || ledgerBytes != expectedBytes || objects != expectedObjects || unrelated != 0 {
		test.Fatal("lifecycle quota account, source, projection, and ledger disagree", namespace, projectedBytes, sourceBytes, ledgerBytes, objects, unrelated)
	}
}

func observeBlobLifecycleWaitPG(test *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID, waiterPID uint32, completed <-chan error) {
	test.Helper()
	authorityScopeWaitPG(test, ctx, observer, blockerPID, waiterPID, "", completed)
	var waitEvent, statement string
	if err := observer.QueryRow(ctx, `SELECT wait_event,query FROM pg_stat_activity
 WHERE pid=$1 AND wait_event_type='Lock' AND $2::integer=ANY(pg_blocking_pids(pid))`, waiterPID, blockerPID).
		Scan(&waitEvent, &statement); err != nil {
		test.Fatal("lifecycle waiter did not retain its exact producer blocker and Lock wait", blockerPID, waiterPID, err)
	}
	test.Logf("blocker_pid=%d waiter_pid=%d wait_event_type=Lock wait_event=%s statement=%s", blockerPID, waiterPID, waitEvent, statement)
}

func blobLifecycleSchedulePG(test *testing.T, rawFirst bool) {
	test.Helper()
	fixture, storage := authorityScopePG(test)
	ctx := fixture.ctx
	docRepo, docNamespace := rootPGRepo(test, ctx, storage)
	rawRepo, rawNamespace := rootPGRepo(test, ctx, storage)
	if docRepo == rawRepo || docNamespace == rawNamespace {
		test.Fatal("lifecycle race requires distinct repositories and quota namespaces")
	}
	document, canonical := blobLifecycleDocumentPG(test)
	if created, err := storage.PutDoc(ctx, docRepo, document); err != nil || !created {
		test.Fatal("seed the sole document owner through public PutDoc", created, err)
	}
	initial := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
	initialBytes, err := docDecompress(initial.body)
	if err != nil || !bytes.Equal(initialBytes, canonical) || domain.HashContent(initialBytes) != document.Hash {
		test.Fatal("public PutDoc did not store legitimate raw canonical bytes", err)
	}
	if _, manifested := domain.ParseDocChunkManifest(initialBytes); manifested {
		test.Fatal("empty-event public document unexpectedly stored a manifest")
	}
	assertBlobLifecycleDocumentPG(test, storage, ctx, docRepo, document, canonical)
	assertBlobLifecycleUsagePG(test, storage, ctx, docNamespace, document.Hash, int64(len(initial.body)))
	assertBlobLifecycleUsagePG(test, storage, ctx, rawNamespace, document.Hash, 0)
	trace := &blobLifecycleDeleteTracePG{}
	deleter := authorityScopePeerPG(test, storage, "read committed", trace)
	rawWriter := authorityScopePeerPG(test, storage, "read committed", nil)
	deletePID := authorityScopePIDPG(test, ctx, deleter)
	rawPID := authorityScopePIDPG(test, ctx, rawWriter)
	if deletePID == rawPID || deletePID == fixture.observer.PgConn().PID() || rawPID == fixture.observer.PgConn().PID() {
		test.Fatal("lifecycle race requires independent delete, raw grant, and observer backends")
	}
	grant := func(bound context.Context) error {
		stored, deduped, err := rawWriter.PutChunks(bound, rawRepo, map[domain.ContentHash][]byte{document.Hash: canonical})
		if err != nil {
			return err
		}
		if stored != 1 || deduped != 0 {
			return fmt.Errorf("one public raw grant returned stored=%d deduped=%d", stored, deduped)
		}
		actual, err := rawWriter.GetChunk(bound, rawRepo, document.Hash)
		if err != nil || !bytes.Equal(actual, canonical) || domain.HashContent(actual) != document.Hash {
			return fmt.Errorf("public raw grant lost its exact current body: %v", err)
		}
		return nil
	}
	remove := func(bound context.Context) error {
		return deleter.DeleteDoc(bound, docRepo, document.Hash)
	}
	holder, holderRepo, holderPID, waiterPID := rawWriter, rawRepo, rawPID, deletePID
	first, second := grant, remove
	if !rawFirst {
		holder, holderRepo, holderPID, waiterPID = deleter, docRepo, deletePID, rawPID
		first, second = remove, grant
	}
	ready, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	held, _, holderDone := authorityScopeRunPG(test, ctx, func(work context.Context) error {
		return holder.WithinRepository(work, holderRepo, func(bound context.Context) error {
			if err := first(bound); err != nil {
				return err
			}
			var backendPID uint32
			var isolation string
			if err := holder.db(bound).QueryRow(bound, `SELECT pg_backend_pid(),current_setting('transaction_isolation')`).Scan(&backendPID, &isolation); err != nil {
				return err
			}
			if !holder.InTransaction(bound) || backendPID != holderPID || isolation != "read committed" {
				return errors.New("public lifecycle operation escaped its borrowed read-committed outer transaction")
			}
			if !rawFirst {
				if trace.deleteCount != 1 || trace.deletedRows != 1 || !trace.outerRetained {
					return fmt.Errorf("DeleteDoc must physically delete the unreferenced source but retain its outer transaction: statements=%d rows=%d retained=%t", trace.deleteCount, trace.deletedRows, trace.outerRetained)
				}
				var grants, bodies, incarnations int
				if err := holder.db(bound).QueryRow(bound, `SELECT
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM blobs WHERE hash=$2),
 (SELECT count(*) FROM blob_source_incarnations WHERE hash=$2)`, docRepo, document.Hash).
					Scan(&grants, &bodies, &incarnations); err != nil {
					return err
				}
				if grants != 0 || bodies != 0 || incarnations != 0 {
					return fmt.Errorf("DeleteDoc did not stage actual source and incarnation removal: grants=%d bodies=%d incarnations=%d", grants, bodies, incarnations)
				}
			}
			close(ready)
			select {
			case <-release:
				return nil
			case <-bound.Done():
				return bound.Err()
			}
		})
	})
	authorityScopeReadyPG(test, held, ready, holderDone)
	assertBlobLifecycleDocumentPG(test, storage, ctx, docRepo, document, canonical)
	assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
	if owned, err := storage.HasChunks(ctx, rawRepo, []domain.ContentHash{document.Hash}); err != nil || len(owned) != 0 {
		test.Fatal("uncommitted raw ownership escaped the borrowed outer transaction", owned, err)
	}
	assertBlobLifecycleUsagePG(test, storage, ctx, docNamespace, document.Hash, int64(len(initial.body)))
	assertBlobLifecycleUsagePG(test, storage, ctx, rawNamespace, document.Hash, 0)
	work, _, waiterDone := authorityScopeRunPG(test, ctx, second)
	observeBlobLifecycleWaitPG(test, work, fixture.observer, holderPID, waiterPID, waiterDone)
	assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
	release <- struct{}{}
	authorityScopeDonePG(test, held, holderDone)
	var waiterErr error
	select {
	case waiterErr = <-waiterDone:
	case <-work.Done():
		test.Fatal("public lifecycle waiter exceeded its finite operation deadline", work.Err())
	}
	actual, err := storage.GetChunk(ctx, rawRepo, document.Hash)
	if err != nil || !bytes.Equal(actual, canonical) || domain.HashContent(actual) != document.Hash {
		test.Fatal("concurrent raw grant lost its exact canonical body or hash", err)
	}
	owned, err := storage.HasChunks(ctx, rawRepo, []domain.ContentHash{document.Hash})
	if err != nil || len(owned) != 1 || owned[0] != document.Hash {
		test.Fatal("concurrent raw ownership is not current", owned, err)
	}
	current := readRootSourceWitnessStatePG(test, storage, ctx, document.Hash)
	currentBytes, err := docDecompress(current.body)
	if err != nil || !bytes.Equal(currentBytes, canonical) || domain.HashContent(currentBytes) != document.Hash {
		test.Fatal("current physical source is not the legitimate raw canonical body", err)
	}
	assertBlobLifecycleUsagePG(test, storage, ctx, rawNamespace, document.Hash, int64(len(current.body)))
	if waiterErr != nil {
		var foreignKey *pgconn.PgError
		if rawFirst && errors.As(waiterErr, &foreignKey) && foreignKey.Code == "23503" {
			assertBlobLifecycleDocumentPG(test, storage, ctx, docRepo, document, canonical)
			assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
			assertBlobLifecycleUsagePG(test, storage, ctx, docNamespace, document.Hash, int64(len(initial.body)))
			test.Fatalf("DeleteDoc must succeed after the raw grant commits; FK correctly rejected deletion and cleanup rolled back, without losing the raw body or grant: %v", waiterErr)
		}
		test.Fatal("public lifecycle waiter must succeed without caller retries", waiterErr)
	}
	if rawFirst {
		assertRootSourceWitnessStatePG(test, storage, ctx, document.Hash, initial)
	} else {
		test.Logf("DeleteDoc released its savepoint with outer retention=%t and physically deleted rows=%d", trace.outerRetained, trace.deletedRows)
		if current.incarnation == initial.incarnation {
			test.Fatal("raw reinsertion after traced physical deletion reused the old source incarnation", current.incarnation)
		}
	}
	for _, repo := range []domain.ContentHash{docRepo, rawRepo} {
		if _, err := storage.GetDoc(ctx, repo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("deleted document grant survived or raw ownership granted document access", repo, err)
		}
		if owned, err := storage.HasDocs(ctx, repo, []domain.ContentHash{document.Hash}); err != nil || len(owned) != 0 {
			test.Fatal("old or foreign document ownership remains current", repo, owned, err)
		}
	}
	if _, err := storage.GetChunk(ctx, docRepo, document.Hash); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("deleted document owner gained the other repository's raw grant", err)
	}
	var docGrants, rawGrants int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$1),
 (SELECT count(*) FROM repo_blobs WHERE repo_id=$2)`, docRepo, rawRepo).Scan(&docGrants, &rawGrants); err != nil || docGrants != 0 || rawGrants != 1 {
		test.Fatal("lifecycle leaked unrelated grants or retained the deleted document owner", docGrants, rawGrants, err)
	}
	assertBlobLifecycleUsagePG(test, storage, ctx, docNamespace, document.Hash, 0)
	assertBlobLifecycleUsagePG(test, storage, ctx, rawNamespace, document.Hash, int64(len(current.body)))
}

func TestPGBlobLifecycleDeleteDocAfterConcurrentRawGrant(test *testing.T) {
	blobLifecycleSchedulePG(test, true)
}

func TestPGBlobLifecycleRawGrantAfterConcurrentDeleteDoc(test *testing.T) {
	blobLifecycleSchedulePG(test, false)
}
