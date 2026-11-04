//go:build postgres

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayMaintenanceState struct {
	source                           domain.ContentHash
	phase                            string
	epoch, revision, currentRevision int64
	indexAfter, jobsRepo, jobsDoc    string
	jobsEvent                        int64
	jobsProfile                      string
	plansScope                       string
	plansParent                      int64
	sweepAfter                       string
}

func (storage *PostgresStore) StepDocDisplayMaintenance(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
	if outbound.DocReadOnly(ctx) || storage.InReadOnlyTransaction(ctx) {
		return domain.DocDisplayMaintenanceStep{}, domain.ErrForbidden
	}
	seen := make(map[domain.ContentHash]bool, docDisplayMaintenanceCandidates)
	for attempt := 0; attempt < docDisplayMaintenanceCandidates; attempt++ {
		if err := ctx.Err(); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		candidate, err := storage.selectDocDisplayMaintenance(ctx)
		if errors.Is(err, errDocDisplayMaintenanceCandidateBusy) {
			if err := ctx.Err(); err != nil {
				return domain.DocDisplayMaintenanceStep{}, err
			}
			return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
		}
		if err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		if seen[candidate.source] {
			return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
		}
		seen[candidate.source] = true
		if !candidate.ready {
			continue
		}
		result, err := storage.stepDocDisplayMaintenanceCandidate(ctx, candidate.source)
		if errors.Is(err, errDocDisplayMaintenanceCandidateBusy) {
			continue
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return domain.DocDisplayMaintenanceStep{}, err
	}
	return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
}

func (storage *PostgresStore) stepDocDisplayMaintenanceCandidate(ctx context.Context, hash domain.ContentHash) (domain.DocDisplayMaintenanceStep, error) {
	transaction, err := storage.beginDocRangeJob(ctx)
	if err != nil {
		return domain.DocDisplayMaintenanceStep{}, err
	}
	defer rollbackPG(transaction)
	source, revision, err := lockDocDisplayMaintenanceSource(ctx, transaction, hash)
	if err != nil {
		return domain.DocDisplayMaintenanceStep{}, docDisplayMaintenanceAcquisitionError(err)
	}
	if err := source.Validate(); err != nil {
		rollbackPG(transaction)
		if ctx.Err() == nil && errors.Is(err, domain.ErrIntegrity) {
			storage.deferDocDisplayMaintenance(context.WithoutCancel(ctx), source.Hash)
		}
		return domain.DocDisplayMaintenanceStep{}, err
	}
	epoch, lifecycle, err := readDocDisplayCatalogState(ctx, transaction, source.Hash)
	if err != nil {
		return domain.DocDisplayMaintenanceStep{}, docDisplayMaintenanceAcquisitionError(err)
	}
	state := docDisplayMaintenanceState{source: source.Hash, currentRevision: revision}
	var ready bool
	err = transaction.QueryRow(ctx, `SELECT phase,epoch,source_revision,index_after,jobs_repo,jobs_doc,jobs_event,jobs_profile,plans_scope,plans_parent,sweep_after,
 due_at<=clock_timestamp() FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE NOWAIT`, state.source).
		Scan(&state.phase, &state.epoch, &state.revision, &state.indexAfter, &state.jobsRepo, &state.jobsDoc, &state.jobsEvent, &state.jobsProfile, &state.plansScope, &state.plansParent, &state.sweepAfter, &ready)
	if err != nil {
		return domain.DocDisplayMaintenanceStep{}, docDisplayMaintenanceAcquisitionError(mapNoRows(err))
	}
	if state.phase == "idle" || !ready {
		return domain.DocDisplayMaintenanceStep{}, errDocDisplayMaintenanceCandidateBusy
	}
	result := domain.DocDisplayMaintenanceStep{Source: state.source}
	if lifecycle == "retiring" {
		if err := retireDocDisplayCatalog(ctx, transaction, source.Hash, epoch, &result); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		return result, nil
	}
	advanced := false
	if lifecycle == "live" {
		advanced, err = censusDocDisplayCatalog(ctx, transaction, source, epoch, state.currentRevision, &result)
	}
	if err != nil {
		rollbackPG(transaction)
		if ctx.Err() == nil && errors.Is(err, domain.ErrIntegrity) {
			storage.deferDocDisplayMaintenance(context.WithoutCancel(ctx), state.source)
		}
		return domain.DocDisplayMaintenanceStep{}, err
	}
	if advanced {
		if _, err := transaction.Exec(ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp() WHERE source_hash=$1`, state.source); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		result.Phase = "catalog_census"
		return result, nil
	}
	if lifecycle == "live" {
		var retiring bool
		if err := transaction.QueryRow(ctx, `SELECT cxthub_begin_doc_catalog_retirement($1,$2::uuid)`, source.Hash, epoch).Scan(&retiring); err != nil {
			return domain.DocDisplayMaintenanceStep{}, err
		}
		if retiring {
			if err := transaction.Commit(ctx); err != nil {
				return domain.DocDisplayMaintenanceStep{}, err
			}
			result.Phase, result.CatalogPhase = "catalog_retirement", "orders"
			return result, nil
		}
	}
	result.Phase = state.phase
	if state.phase != "index" && state.currentRevision != state.revision {
		err = state.nextEpoch()
	}
	if err == nil {
		switch state.phase {
		case "index":
			err = storage.indexDocDisplayMaintenance(ctx, transaction, &state, &result)
		case "roots_jobs":
			err = storage.rootDocDisplayJobs(ctx, transaction, &state)
		case "roots_generation":
			err = storage.rootDocDisplayGeneration(ctx, transaction, &state)
		case "roots_trees":
			err = storage.rootDocDisplayTrees(ctx, transaction, &state)
		case "roots_plans":
			err = storage.rootDocDisplayPlans(ctx, transaction, &state)
		case "mark":
			err = storage.markDocDisplayObjects(ctx, transaction, &state, &result)
		case "sweep":
			err = storage.sweepDocDisplayObjects(ctx, transaction, &state, &result)
		default:
			err = domain.ErrIntegrity
		}
	}
	if err != nil {
		rollbackPG(transaction)
		if ctx.Err() == nil {
			storage.deferDocDisplayMaintenance(context.WithoutCancel(ctx), state.source)
		}
		return domain.DocDisplayMaintenanceStep{}, err
	}
	_, err = transaction.Exec(ctx, `UPDATE doc_display_maintenance_v1 SET phase=$2,epoch=$3,source_revision=$4,index_after=$5,
 jobs_repo=$6,jobs_doc=$7,jobs_event=$8,jobs_profile=$9,plans_scope=$10,plans_parent=$11,sweep_after=$12,due_at=clock_timestamp()
 WHERE source_hash=$1`, state.source, state.phase, state.epoch, state.revision, state.indexAfter, state.jobsRepo, state.jobsDoc, state.jobsEvent, state.jobsProfile, state.plansScope, state.plansParent, state.sweepAfter)
	if err != nil {
		return domain.DocDisplayMaintenanceStep{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return domain.DocDisplayMaintenanceStep{}, err
	}
	result.Phase = state.phase
	return result, nil
}

var _ outbound.DocDisplayMaintenanceStore = (*PostgresStore)(nil)

func (state *docDisplayMaintenanceState) nextEpoch() error {
	if state.epoch == 9223372036854775807 {
		return domain.ErrIntegrity
	}
	state.epoch++
	state.restartRoots()
	return nil
}

func (state *docDisplayMaintenanceState) restartRoots() {
	state.phase, state.revision = "roots_jobs", state.currentRevision
	state.jobsRepo, state.jobsDoc, state.jobsEvent = "", "", -1
	state.jobsProfile = ""
	state.plansScope, state.plansParent, state.sweepAfter = "", -1, ""
}

func (storage *PostgresStore) deferDocDisplayMaintenance(parent context.Context, source domain.ContentHash) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	transaction, err := storage.beginDocRangeJob(ctx)
	if err != nil {
		return
	}
	defer rollbackPG(transaction)
	if _, _, err := lockDocDisplayMaintenanceSource(ctx, transaction, source); err != nil {
		return
	}
	if _, _, err := readDocDisplayCatalogState(ctx, transaction, source); err != nil {
		return
	}
	if _, err := transaction.Exec(ctx, `SELECT source_hash FROM doc_display_maintenance_v1 WHERE source_hash=$1 FOR UPDATE NOWAIT`, source); err != nil {
		return
	}
	if _, err := transaction.Exec(ctx, `UPDATE doc_display_maintenance_v1 SET due_at=clock_timestamp()+interval '1 minute' WHERE source_hash=$1`, source); err != nil {
		return
	}
	_ = transaction.Commit(ctx)
}

func (storage *PostgresStore) indexDocDisplayMaintenance(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, result *domain.DocDisplayMaintenanceStep) error {
	const batch = domain.MaxDocEventDisplayStepBytes / domain.MaxDocDisplayPageBytes
	rows, err := transaction.Query(ctx, `SELECT hash,substring(bytes FROM 1 FOR $3) FROM doc_display_objects_v1
 WHERE source_hash=$1 AND references_version=0 AND hash>$2 ORDER BY hash LIMIT $4`, state.source, state.indexAfter, domain.MaxDocDisplayPageBytes+1, batch)
	if err != nil {
		return err
	}
	var objects []domain.DocRangeObject
	for rows.Next() {
		var object domain.DocRangeObject
		if err := rows.Scan(&object.Hash, &object.Bytes); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, object)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := indexDocDisplayObjectReferences(ctx, transaction, state.source, object.Hash, object.Bytes); err != nil {
			return err
		}
		state.indexAfter = string(object.Hash)
		result.Indexed++
	}
	if len(objects) != 0 {
		return nil
	}
	var remains bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_objects_v1 WHERE source_hash=$1 AND references_version=0)`, state.source).Scan(&remains); err != nil {
		return err
	}
	if remains {
		state.indexAfter = ""
		return nil
	}
	state.indexAfter = ""
	return state.nextEpoch()
}

func markDocDisplayCandidates(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, candidates []domain.ContentHash) error {
	if len(candidates) == 0 {
		return nil
	}
	_, err := transaction.Exec(ctx, `INSERT INTO doc_display_object_marks_v1(source_hash,hash,epoch,expanded)
 SELECT source_hash,hash,$3::bigint,false FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=ANY($2::text[])
 ON CONFLICT(source_hash,hash) DO UPDATE SET epoch=EXCLUDED.epoch,expanded=false
 WHERE doc_display_object_marks_v1.epoch<>EXCLUDED.epoch`, state.source, candidates, state.epoch)
	return err
}

func markDocDisplayRoot(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	candidates, err := domain.DocDisplayHashCandidates(ctx, raw)
	if err != nil {
		return err
	}
	return markDocDisplayCandidates(ctx, transaction, state, candidates)
}

func markDocDisplayRootProof(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, hash domain.ContentHash, raw []byte) error {
	if domain.ValidateContentHash(hash) != nil || len(raw) == 0 || domain.HashContent(raw) != hash {
		return domain.ErrIntegrity
	}
	return markDocDisplayRoot(ctx, transaction, state, raw)
}

func (storage *PostgresStore) rootDocDisplayJobs(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState) error {
	const batch = domain.MaxDocEventDisplayStepBytes / domain.MaxDocDisplayPageBytes
	rows, err := transaction.Query(ctx, `SELECT repo_id,doc_hash,event_index,profile,checkpoint_hash,substring(checkpoint FROM 1 FOR $6),preparation_hash,substring(preparation FROM 1 FOR $7)
 FROM doc_display_jobs_v1 WHERE source_hash=$1 AND (repo_id,doc_hash,event_index,profile)>($2,$3,$4,$5)
 ORDER BY repo_id,doc_hash,event_index,profile LIMIT $8`, state.source, state.jobsRepo, state.jobsDoc, state.jobsEvent, state.jobsProfile, domain.MaxDocDisplayStateBytes+1, domain.MaxDocEventDisplayPreparationBytes+1, batch)
	if err != nil {
		return err
	}
	type root struct {
		repo, doc               string
		event                   int64
		profile                 string
		checkpointHash          *domain.ContentHash
		preparationHash         *domain.ContentHash
		checkpoint, preparation []byte
	}
	var roots []root
	for rows.Next() {
		var value root
		if err := rows.Scan(&value.repo, &value.doc, &value.event, &value.profile, &value.checkpointHash, &value.checkpoint, &value.preparationHash, &value.preparation); err != nil {
			rows.Close()
			return err
		}
		roots = append(roots, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, root := range roots {
		if len(root.checkpoint) > domain.MaxDocDisplayStateBytes || len(root.preparation) > domain.MaxDocEventDisplayPreparationBytes {
			return domain.ErrIntegrity
		}
		for index, hash := range []*domain.ContentHash{root.checkpointHash, root.preparationHash} {
			raw := [][]byte{root.checkpoint, root.preparation}[index]
			if hash == nil {
				if len(raw) != 0 {
					return domain.ErrIntegrity
				}
				continue
			}
			if err := markDocDisplayRootProof(ctx, transaction, state, *hash, raw); err != nil {
				return err
			}
		}
		state.jobsRepo, state.jobsDoc, state.jobsEvent = root.repo, root.doc, root.event
		state.jobsProfile = root.profile
	}
	if len(roots) == 0 {
		state.phase = "roots_generation"
	}
	return nil
}

func (storage *PostgresStore) rootDocDisplayGeneration(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState) error {
	var raw []byte
	var hash domain.ContentHash
	err := transaction.QueryRow(ctx, `SELECT generation_hash,substring(bytes FROM 1 FOR $2) FROM doc_display_generations_v1 WHERE source_hash=$1`, state.source, domain.MaxDocDisplaySourceBytes+1).Scan(&hash, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if len(raw) > domain.MaxDocDisplaySourceBytes {
		return domain.ErrIntegrity
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if err := markDocDisplayRootProof(ctx, transaction, state, hash, raw); err != nil {
			return err
		}
	}
	state.phase = "roots_trees"
	return nil
}

func (storage *PostgresStore) rootDocDisplayTrees(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState) error {
	rows, err := transaction.Query(ctx, `SELECT scope,substring(bytes FROM 1 FOR $3) FROM doc_tool_tree_sources_v1
 WHERE source_hash=$1 AND scope>$2 ORDER BY scope LIMIT $4`, state.source, state.plansScope, domain.MaxDocDisplaySourceBytes+1, domain.DocDisplayMaintenanceBatch)
	if err != nil {
		return err
	}
	type root struct {
		scope string
		raw   []byte
	}
	var roots []root
	for rows.Next() {
		var value root
		if err := rows.Scan(&value.scope, &value.raw); err != nil {
			rows.Close()
			return err
		}
		roots = append(roots, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, root := range roots {
		if len(root.raw) > domain.MaxDocDisplaySourceBytes || domain.HashContent(root.raw) != domain.ContentHash(root.scope) {
			return domain.ErrIntegrity
		}
		if err := markDocDisplayRoot(ctx, transaction, state, root.raw); err != nil {
			return err
		}
		state.plansScope = root.scope
	}
	if len(roots) == 0 {
		state.phase = "roots_plans"
		state.plansScope = ""
	}
	return nil
}

func (storage *PostgresStore) rootDocDisplayPlans(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState) error {
	if state.plansParent == -1 {
		var scope string
		var treeScope, keysScope domain.ContentHash
		var orderRaw, treeRaw, keysRaw []byte
		err := transaction.QueryRow(ctx, `SELECT source.scope,source.tree_scope,source.keys_scope,substring(source.bytes FROM 1 FOR $3),substring(tree.bytes FROM 1 FOR $3),substring(keys.bytes FROM 1 FOR $3)
 FROM doc_tool_order_sources_v1 source JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope
 JOIN doc_tool_key_sources_v1 keys ON keys.scope=source.keys_scope AND keys.tree_scope=source.tree_scope
 WHERE source.source_hash=$1 AND source.scope>$2 ORDER BY source.scope LIMIT 1`, state.source, state.plansScope, domain.MaxDocDisplaySourceBytes+1).Scan(&scope, &treeScope, &keysScope, &orderRaw, &treeRaw, &keysRaw)
		if errors.Is(err, pgx.ErrNoRows) {
			state.phase = "mark"
			return nil
		}
		if err != nil {
			return err
		}
		for index, raw := range [][]byte{orderRaw, treeRaw, keysRaw} {
			if len(raw) > domain.MaxDocDisplaySourceBytes {
				return domain.ErrIntegrity
			}
			hash := []domain.ContentHash{domain.ContentHash(scope), treeScope, keysScope}[index]
			if err := markDocDisplayRootProof(ctx, transaction, state, hash, raw); err != nil {
				return err
			}
		}
		state.plansScope, state.plansParent = scope, 0
	}
	rows, err := transaction.Query(ctx, `SELECT parent_from,plan_hash,substring(bytes FROM 1 FOR $4)
 FROM doc_tool_order_plans_v1 WHERE scope=$1 AND parent_from>=$2 ORDER BY parent_from LIMIT $3`, state.plansScope, state.plansParent, domain.DocDisplayMaintenanceBatch, domain.MaxDocDisplaySourceBytes+1)
	if err != nil {
		return err
	}
	type root struct {
		parent int64
		hash   domain.ContentHash
		raw    []byte
	}
	var roots []root
	for rows.Next() {
		var value root
		if err := rows.Scan(&value.parent, &value.hash, &value.raw); err != nil {
			rows.Close()
			return err
		}
		roots = append(roots, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, root := range roots {
		if len(root.raw) > domain.MaxDocDisplaySourceBytes || domain.HashContent(root.raw) != root.hash {
			return domain.ErrIntegrity
		}
		if err := markDocDisplayCandidates(ctx, transaction, state, []domain.ContentHash{root.hash}); err != nil {
			return err
		}
		if err := markDocDisplayRoot(ctx, transaction, state, root.raw); err != nil {
			return err
		}
		state.plansParent = root.parent + 1
	}
	if len(roots) == 0 {
		state.plansParent = -1
	}
	return nil
}

func (storage *PostgresStore) markDocDisplayObjects(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, result *domain.DocDisplayMaintenanceStep) error {
	var unindexed bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_objects_v1 WHERE source_hash=$1 AND references_version=0)`, state.source).Scan(&unindexed); err != nil {
		return err
	}
	if unindexed {
		state.phase, state.indexAfter = "index", ""
		return nil
	}
	rows, err := transaction.Query(ctx, `SELECT hash FROM doc_display_object_marks_v1 WHERE source_hash=$1 AND epoch=$2 AND NOT expanded ORDER BY hash LIMIT $3`, state.source, state.epoch, domain.DocDisplayMaintenanceBatch)
	if err != nil {
		return err
	}
	var hashes []domain.ContentHash
	for rows.Next() {
		var hash domain.ContentHash
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(hashes) == 0 {
		state.phase, state.sweepAfter = "sweep", ""
		return nil
	}
	_, err = transaction.Exec(ctx, `INSERT INTO doc_display_object_marks_v1(source_hash,hash,epoch,expanded)
 SELECT DISTINCT child.source_hash,child.hash,$3::bigint,false FROM doc_display_object_edges_v1 edge
 JOIN doc_display_objects_v1 child ON child.source_hash=edge.source_hash AND child.hash=edge.child_hash
 WHERE edge.source_hash=$1 AND edge.parent_hash=ANY($2::text[])
 ON CONFLICT(source_hash,hash) DO UPDATE SET epoch=EXCLUDED.epoch,expanded=false WHERE doc_display_object_marks_v1.epoch<>EXCLUDED.epoch`, state.source, hashes, state.epoch)
	if err != nil {
		return err
	}
	_, err = transaction.Exec(ctx, `UPDATE doc_display_object_marks_v1 SET expanded=true WHERE source_hash=$1 AND epoch=$2 AND hash=ANY($3::text[])`, state.source, state.epoch, hashes)
	result.Marked = len(hashes)
	return err
}

func (storage *PostgresStore) sweepDocDisplayObjects(ctx context.Context, transaction pgx.Tx, state *docDisplayMaintenanceState, result *domain.DocDisplayMaintenanceStep) error {
	var unindexed bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_objects_v1 WHERE source_hash=$1 AND references_version=0)`, state.source).Scan(&unindexed); err != nil {
		return err
	}
	if unindexed {
		state.phase, state.indexAfter = "index", ""
		return nil
	}
	rows, err := transaction.Query(ctx, `SELECT hash FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash>$2 ORDER BY hash LIMIT $3`, state.source, state.sweepAfter, domain.DocDisplayMaintenanceBatch)
	if err != nil {
		return err
	}
	var hashes []domain.ContentHash
	for rows.Next() {
		var hash domain.ContentHash
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(hashes) == 0 {
		state.phase = "idle"
		return nil
	}
	deleted, err := transaction.Query(ctx, `DELETE FROM doc_display_objects_v1 object WHERE source_hash=$1 AND hash=ANY($2::text[])
 AND NOT EXISTS(SELECT 1 FROM doc_display_object_marks_v1 mark WHERE mark.source_hash=object.source_hash AND mark.hash=object.hash AND mark.epoch=$3)
 RETURNING octet_length(bytes)`, state.source, hashes, state.epoch)
	if err != nil {
		return err
	}
	for deleted.Next() {
		var size int64
		if err := deleted.Scan(&size); err != nil {
			deleted.Close()
			return err
		}
		result.Deleted++
		result.DeletedBytes += size
	}
	err = deleted.Err()
	deleted.Close()
	if err != nil {
		return err
	}
	state.sweepAfter = string(hashes[len(hashes)-1])
	return nil
}
