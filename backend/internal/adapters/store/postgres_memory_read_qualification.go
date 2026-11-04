//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.MemoryReadQualificationStore = (*PostgresStore)(nil)

func (storage *PostgresStore) QualifyMemoryRead(ctx context.Context, qualification domain.MemoryReadQualification) (identity domain.ContentHash, failure error) {
	defer func() {
		var databaseError *pgconn.PgError
		if errors.As(failure, &databaseError) && (databaseError.Code == "55P03" || databaseError.Code == "40001" || databaseError.Code == "40P01") {
			failure = domain.ErrConflict
		}
		if errors.As(failure, &databaseError) && databaseError.ConstraintName == "memory_read_qualification_capture_live" {
			failure = domain.ErrMemoryCaptureExpired
		}
	}()
	if err := storage.memoryCaptureOwnWrite(ctx); err != nil {
		return "", err
	}
	if outbound.DocReadOnly(ctx) {
		return "", domain.ErrConflict
	}
	raw, err := domain.MemoryReadQualificationBytes(ctx, qualification)
	if err != nil {
		return "", err
	}
	var owned domain.MemoryReadQualification
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &owned); err != nil {
		return "", err
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer rollbackPG(transaction)
	identifier, err := qualifyMemoryReadInTx(ctx, transaction, owned, raw, nil)
	if err != nil {
		return "", err
	}
	if err := transaction.Commit(ctx); err != nil {
		return "", err
	}
	return identifier, nil
}

func qualifyMemoryReadInTx(ctx context.Context, transaction pgx.Tx, owned domain.MemoryReadQualification, raw []byte, target *domain.MemoryReadTarget) (domain.ContentHash, error) {
	identifier, _, err := qualifyMemoryReadAtMinimumInTx(ctx, transaction, owned, raw, target, nil)
	return identifier, err
}

func qualifyMemoryReadAtMinimumInTx(ctx context.Context, transaction pgx.Tx, owned domain.MemoryReadQualification, raw []byte, target *domain.MemoryReadTarget, minimum *domain.RepositoryMemorySourceGeneration) (domain.ContentHash, bool, error) {
	repo, identifier := owned.Source.Graph.RepoID, domain.HashContent(raw)
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('cxt-memory-capture:' || $1,0))`, string(repo)).Scan(&locked); err != nil {
		return "", false, err
	}
	if !locked {
		return "", false, domain.ErrConflict
	}
	current, err := readMemoryQualificationSource(ctx, transaction, repo)
	if err != nil {
		return "", false, err
	}
	if minimum == nil {
		if current != owned.Source {
			return "", false, domain.ErrConflict
		}
	} else {
		if err := domain.CheckMemorySourceMinimum(owned.Source, *minimum); err != nil {
			return "", false, err
		}
		if err := domain.CheckMemorySourceMinimum(current, owned.Source); err != nil {
			return "", false, err
		}
	}
	var readID string
	if err := transaction.QueryRow(ctx, `SELECT read_id FROM memory_read_headers WHERE repo_id=$1 AND read_id=$2 FOR SHARE NOWAIT`, string(repo), string(owned.ReadID)).Scan(&readID); err != nil {
		return "", false, mapNoRows(err)
	}
	header, _, err := readMemoryReadDescriptor(ctx, transaction, repo, owned.ReadID)
	if err != nil {
		return "", false, err
	}
	if err := domain.ValidateMemoryReadQualification(ctx, owned, header); err != nil {
		return "", false, err
	}
	if target != nil && (header.Source.Graph.RepoID != target.RepoID || header.Rules != target.Rules || header.View.Selection != target.Selection || header.View.Content != target.Content) {
		return "", false, domain.ErrConflict
	}
	var captureID string
	if err := transaction.QueryRow(ctx, `SELECT capture_id FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2 FOR SHARE NOWAIT`, string(repo), string(owned.CaptureID)).Scan(&captureID); err != nil {
		return "", false, mapNoRows(err)
	}
	if err := lockMemoryQualificationObjects(ctx, transaction, repo, owned.Witnesses); err != nil {
		return "", false, err
	}
	if minimum == nil {
		err = validateMemoryObjectWitnesses(ctx, transaction, repo, owned.Witnesses)
	} else {
		err = validateMinimumMemoryObjectWitnesses(ctx, transaction, repo, owned.Witnesses)
	}
	if err != nil {
		return "", false, err
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO memory_read_qualifications(repo_id,qualification_id,read_id,body)
 VALUES($1,$2,$3,$4) ON CONFLICT(repo_id,qualification_id) DO NOTHING`, string(repo), string(identifier), string(owned.ReadID), raw); err != nil {
		return "", false, err
	}
	var persistedID string
	if err := transaction.QueryRow(ctx, `SELECT qualification_id FROM memory_read_qualifications WHERE repo_id=$1 AND qualification_id=$2 FOR SHARE NOWAIT`, string(repo), string(identifier)).Scan(&persistedID); err != nil {
		return "", false, mapNoRows(err)
	}
	stored, _, err := readMemoryQualification(ctx, transaction, repo, identifier)
	if err != nil {
		return "", false, err
	}
	if !bytes.Equal(stored, raw) {
		return "", false, domain.ErrIntegrity
	}
	if err := memoryReadCheckExpiry(ctx, transaction, repo, owned.CaptureID); err != nil {
		return "", false, err
	}
	return identifier, current == owned.Source, nil
}

func validateMinimumMemoryObjectWitnesses(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, witnesses []domain.MemoryObjectWitness) error {
	if len(witnesses) > domain.MaxMemoryCaptureObjects {
		return domain.ErrMemoryProjectionLimit
	}
	set := domain.NewMemoryObjectWitnessSet(repo)
	available := make([]domain.MemoryObjectWitness, 0, len(witnesses))
	for _, witness := range witnesses {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := set.Add(witness); err != nil {
			return err
		}
		if witness.Available {
			available = append(available, witness)
		}
	}
	return validateMemoryObjectWitnesses(ctx, transaction, repo, available)
}

func fenceMemoryQualificationSource(ctx context.Context, transaction pgx.Tx, expected domain.RepositoryMemorySourceGeneration) error {
	actual, err := readMemoryQualificationSource(ctx, transaction, expected.Graph.RepoID)
	if err != nil {
		return err
	}
	if actual != expected {
		return domain.ErrConflict
	}
	return nil
}

func readMemoryQualificationSource(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error) {
	var required domain.DocumentIdentity
	if err := transaction.QueryRow(ctx, `SELECT required_doc_identity FROM repos WHERE id=$1 FOR SHARE NOWAIT`, string(repo)).Scan(&required); err != nil {
		return domain.RepositoryMemorySourceGeneration{}, mapNoRows(err)
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, required); err != nil {
		return domain.RepositoryMemorySourceGeneration{}, err
	}
	actual := domain.RepositoryMemorySourceGeneration{Version: domain.RepositoryMemorySourceVersion,
		Graph: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo}}
	var sourceEpoch string
	var floor, baseline int64
	if err := transaction.QueryRow(ctx, repositoryGraphSourceStateSQL+` FOR SHARE OF catalog,source,graph NOWAIT`, string(repo)).
		Scan(&actual.Graph.Epoch, &actual.Graph.Sequence, &floor, &sourceEpoch, &baseline); err != nil {
		return domain.RepositoryMemorySourceGeneration{}, mapNoRows(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT epoch::text,head_seq FROM repository_memory_source_state WHERE repo_id=$1 FOR SHARE NOWAIT`, string(repo)).
		Scan(&actual.Epoch, &actual.Sequence); err != nil {
		return domain.RepositoryMemorySourceGeneration{}, mapNoRows(err)
	}
	if actual.Validate() != nil || actual.Graph.Epoch != sourceEpoch || floor < 0 || baseline < 0 || floor > actual.Graph.Sequence || baseline > actual.Graph.Sequence {
		return domain.RepositoryMemorySourceGeneration{}, domain.ErrIntegrity
	}
	return actual, nil
}

func lockMemoryQualificationObjects(ctx context.Context, transaction pgx.Tx, repo domain.ContentHash, witnesses []domain.MemoryObjectWitness) error {
	set := domain.NewMemoryObjectWitnessSet(repo)
	for _, witness := range witnesses {
		if err := set.Add(witness); err != nil {
			return err
		}
	}
	objects, err := set.Objects()
	if err != nil {
		return err
	}
	for offset := 0; offset < len(objects); offset += 128 {
		batch := objects[offset:min(offset+128, len(objects))]
		var kinds, hashes []string
		for _, object := range batch {
			if object.Version != nil {
				kinds, hashes = append(kinds, object.Kind), append(hashes, string(object.Hash))
			}
		}
		if len(hashes) == 0 {
			continue
		}
		rows, err := transaction.Query(ctx, `SELECT owned.hash FROM unnest($2::text[],$3::text[]) wanted(kind,hash)
 JOIN repo_blobs owned ON owned.repo_id=$1 AND owned.kind=wanted.kind AND owned.hash=wanted.hash
 JOIN blobs body ON body.hash=owned.hash JOIN blob_source_incarnations source ON source.hash=body.hash
 ORDER BY body.hash,owned.kind FOR SHARE OF owned,body,source NOWAIT`, string(repo), kinds, hashes)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if count != len(hashes) {
			return domain.ErrConflict
		}
	}
	return ctx.Err()
}

func readMemoryQualification(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) ([]byte, domain.MemoryReadQualification, error) {
	var raw []byte
	var size int64
	var readID *string
	if err := transaction.QueryRow(ctx, `SELECT octet_length(body),CASE WHEN octet_length(body)<=$3 THEN body END,
 CASE WHEN octet_length(read_id)=71 THEN read_id END FROM memory_read_qualifications WHERE repo_id=$1 AND qualification_id=$2`,
		string(repo), string(identifier), domain.MaxMemoryReadQualificationBytes).Scan(&size, &raw, &readID); err != nil {
		return nil, domain.MemoryReadQualification{}, mapNoRows(err)
	}
	if size > domain.MaxMemoryReadQualificationBytes {
		return nil, domain.MemoryReadQualification{}, domain.ErrMemoryProjectionLimit
	}
	if size < 1 || int64(len(raw)) != size || readID == nil || domain.HashContent(raw) != identifier {
		return nil, domain.MemoryReadQualification{}, domain.ErrIntegrity
	}
	var qualification domain.MemoryReadQualification
	if err := domain.DecodeRepositorySourceJSON(ctx, raw, &qualification); err != nil {
		return nil, domain.MemoryReadQualification{}, err
	}
	canonical, err := domain.MemoryReadQualificationBytes(ctx, qualification)
	if err != nil {
		return nil, domain.MemoryReadQualification{}, err
	}
	if !bytes.Equal(canonical, raw) || qualification.Source.Graph.RepoID != repo || qualification.ReadID != domain.ContentHash(*readID) {
		return nil, domain.MemoryReadQualification{}, domain.ErrIntegrity
	}
	return raw, qualification, nil
}

func (storage *PostgresStore) WithQualifiedMemoryReadRange(ctx context.Context, repo, identifier domain.ContentHash, authorize func(context.Context) (context.Context, error), consume func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error) error {
	if consume == nil || authorize == nil {
		return domain.ErrValidation
	}
	if storage != nil && storage.InTransaction(ctx) {
		return domain.ErrConflict
	}
	transaction, _, err := storage.memoryReadSnapshot(ctx, repo, identifier)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	snapshot := &repositoryTx{Tx: transaction, owner: storage, readOnly: true, repo: repo}
	ctx, err = authorize(context.WithValue(ctx, repositoryTxKey{}, snapshot))
	if err != nil {
		return err
	}
	if ctx == nil || ctx.Value(repositoryTxKey{}) != snapshot {
		return domain.ErrConflict
	}
	header, verifier, err := storage.qualifiedMemoryReadInTx(ctx, transaction, repo, identifier)
	if err != nil {
		return err
	}
	raw, err := domain.MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return err
	}
	if err := consumeMemoryReadRange(ctx, transaction, repo, domain.HashContent(raw), header, verifier, consume); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (storage *PostgresStore) qualifiedMemoryReadInTx(ctx context.Context, transaction pgx.Tx, repo, identifier domain.ContentHash) (domain.MemoryReadHeader, *domain.MemoryReadChunkVerifier, error) {
	_, qualification, err := readMemoryQualification(ctx, transaction, repo, identifier)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	header, verifier, err := readMemoryReadDescriptor(ctx, transaction, repo, qualification.ReadID)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	if err := domain.ValidateMemoryReadQualification(ctx, qualification, header); err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	bound := ctx
	current, err := storage.ReadRepositoryMemorySourceGeneration(bound, repo)
	if err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	if current != qualification.Source {
		return domain.MemoryReadHeader{}, nil, domain.ErrConflict
	}
	if err := storage.ValidateMemoryObjectWitnesses(bound, repo, qualification.Witnesses); err != nil {
		return domain.MemoryReadHeader{}, nil, err
	}
	return header, verifier, nil
}
