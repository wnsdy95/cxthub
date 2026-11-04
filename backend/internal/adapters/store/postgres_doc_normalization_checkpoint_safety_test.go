//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func normalizationCheckpointPGReplaceOutput(test *testing.T, ctx context.Context, checkpoint *legacyNormalizationCheckpointWork, body []byte) legacyByteCheckpoint {
	test.Helper()
	root := checkpoint.root
	file := root.State.Files[3]
	if len(body) != file.Size || file.BufferBytes != 0 || file.PageBytes != 0 || root.State.Verified != file.Parts {
		test.Fatal("output replacement must preserve the actual Ready control geometry")
	}
	file.Generation++
	root.State.Files[3] = file
	var data legacyNormalizationPrefix
	var err error
	for data.Tree.DataBytes < len(body) {
		data, err = checkpoint.store.stageLegacyNormalizationPrefix(ctx, checkpoint.job, checkpoint.state, data, file.Generation,
			domain.LegacyCanonicalEncodingBytes, len(body), func(ctx context.Context, offset, length int) ([]byte, error) {
				return body[offset : offset+length], ctx.Err()
			})
		if err != nil {
			test.Fatal(err)
		}
	}
	receipts := make([]byte, file.Parts*legacyNormalizationReceiptBytes)
	for ordinal := 0; ordinal < file.Parts; ordinal++ {
		first := ordinal * domain.ConversationManifestChunkBytes
		part := body[first:min(first+domain.ConversationManifestChunkBytes, len(body))]
		position := ordinal * legacyNormalizationReceiptBytes
		binary.BigEndian.PutUint32(receipts[position:], uint32(len(part)))
		hash := domain.HashContent(part)
		if _, err := hex.Decode(receipts[position+4:position+legacyNormalizationReceiptBytes], []byte(hash[7:])); err != nil {
			test.Fatal(err)
		}
	}
	parts, err := checkpoint.store.stageLegacyNormalizationPrefix(ctx, checkpoint.job, checkpoint.state, legacyNormalizationPrefix{}, file.Generation,
		legacyNormalizationReceiptChunkBytes, len(receipts), func(ctx context.Context, offset, length int) ([]byte, error) {
			return receipts[offset : offset+length], ctx.Err()
		})
	if err != nil {
		test.Fatal(err)
	}
	root.Files[3].Data, root.Files[3].Parts = data, parts
	if !root.valid(checkpoint.state) {
		test.Fatal("altered derived output unexpectedly lost its independently valid control/receipt shape")
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		test.Fatal(err)
	}
	object := legacyByteCheckpointObject{hash: domain.HashContent(encoded), data: encoded, structure: 2}
	state, err := checkpoint.store.saveLegacyNormalizationCheckpoint(ctx, checkpoint.job, checkpoint.state, object.hash, len(encoded), []legacyByteCheckpointObject{object})
	if err != nil {
		test.Fatal(err)
	}
	return state
}

func normalizationCheckpointPGReclaim(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) domain.DocRangeIndexJob {
	test.Helper()
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND version=$3`, job.RepoID, job.DocHash, job.Version); err != nil {
		test.Fatal(err)
	}
	return docRangeJobPGClaim(test, storage, ctx, job.RepoID, job.DocHash)
}

func TestNormalizationCheckpointPGPreservesLocalDamageAndRejectsDerivedProof(test *testing.T) {
	for _, damage := range []string{"local-spool", "self-consistent-derived-output"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			rangeSpoolTempDir(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 768<<10)
			producer, job, _ := docNormalizationPGStart(test, storage, ctx, fixture)
			defer producer.Close()
			normalizationCheckpointPGCut(test, producer, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				return checkpoint.state.hash != "" && !checkpoint.busy() && producer.normalization.prepared
			})
			state := producer.checkpoint.state
			if damage == "self-consistent-derived-output" {
				body := bytes.Clone(fixture.raw)
				position := bytes.Index(body, []byte("xxxxxxxx"))
				if position < 0 {
					test.Fatal("fixture lost its mutable text")
				}
				body[position] = 'y'
				state = normalizationCheckpointPGReplaceOutput(test, ctx, producer.checkpoint, body)
			}
			peer := producer.store
			if err := producer.Close(); err != nil {
				test.Fatal(err)
			}
			current := normalizationCheckpointPGReclaim(test, peer, ctx, job)
			prepared, err := peer.PrepareDocRangeWork(ctx, current)
			if err != nil || prepared == nil {
				test.Fatal(err)
			}
			defer prepared.Close()
			work := prepared.(*preparedLegacyCaptureWork)
			if damage == "local-spool" {
				normalizationCheckpointPGCut(test, work, ctx, current, func(checkpoint *legacyNormalizationCheckpointWork) bool {
					return checkpoint.restoring && checkpoint.restoreSlot == domain.LegacyNormalizationSlots && checkpoint.restore == nil
				})
				if _, err := work.normalization.storage.spools[3].file.WriteAt([]byte("x"), 0); err != nil {
					test.Fatal(err)
				}
			}
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			var failure error
			for step := 0; step < normalizationCheckpointPGStepLimit; step++ {
				progress, err := work.Step(ctx, current)
				if err != nil {
					failure = err
					break
				}
				if progress != outbound.DocRangeWorkPending {
					test.Fatal("damaged local/derived output became publishable", progress)
				}
			}
			if !errors.Is(failure, domain.ErrPreparedDocRangeObject) {
				test.Fatal("derived corruption was attributed to the immutable original, or never rejected", failure)
			}
			actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, current)
			if err != nil || damage == "local-spool" && (!found || actual != state || !errors.Is(failure, errLegacyByteRecoveryLocal)) ||
				damage != "local-spool" && found || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("damage handling discarded a healthy root, retained a false proof, or changed original/public state", actual, found, failure, err)
			}
			if err := work.Close(); err != nil {
				test.Fatal(err)
			}
			retry, err := peer.PrepareDocRangeWork(ctx, current)
			if err != nil || retry == nil {
				test.Fatal("automatic retry could not prepare from preserved checkpoint/original", err)
			}
			defer retry.Close()
			progress := outbound.DocRangeWorkPending
			for step := 0; step < normalizationCheckpointPGStepLimit && progress == outbound.DocRangeWorkPending; step++ {
				progress, err = retry.Step(ctx, current)
				if err != nil {
					test.Fatal("automatic retry failed", err)
				}
			}
			if progress != outbound.DocRangeWorkPublishable {
				test.Fatal("automatic retry never reached publication")
			}
			if err := retry.Publish(ctx, current); err != nil {
				test.Fatal(err)
			}
			legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
		})
	}
}

func TestNormalizationCheckpointPGCompletedStageRetiresRoot(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 768<<10)
	producer, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
	defer producer.Close()
	for step := 0; step < normalizationCheckpointPGStepLimit && !legacyByteRecoveryPGReady(test, storage, ctx, fixture.hash); step++ {
		if progress, err := producer.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("normalization did not reach the durable canonical handoff", progress, err)
		}
	}
	if !legacyByteRecoveryPGReady(test, storage, ctx, fixture.hash) {
		test.Fatal("canonical handoff did not finish within the finite schedule")
	}
	if _, pinned, err := storage.loadLegacyNormalizationCheckpoint(ctx, job); err != nil || !pinned {
		test.Fatal("fixture missed the crash boundary before checkpoint retirement", pinned, err)
	}
	peer := producer.store
	if err := producer.Close(); err != nil {
		test.Fatal(err)
	}
	current := normalizationCheckpointPGReclaim(test, peer, ctx, job)
	queries := trace.bodyReads.Load()
	recovery, err := peer.PrepareDocRangeWork(ctx, current)
	if err != nil || recovery == nil {
		test.Fatal(err)
	}
	defer recovery.Close()
	if _, completed := recovery.(*preparedLegacyRecoveryWork); !completed {
		test.Fatalf("completed canonical stage lost priority to earlier normalization state: %T", recovery)
	}
	if _, pinned, err := storage.loadLegacyNormalizationCheckpoint(ctx, current); err != nil || pinned {
		test.Fatal("completed canonical recovery leaked its old processing checkpoint", pinned, err)
	}
	progress := outbound.DocRangeWorkPending
	for step := 0; step < normalizationCheckpointPGStepLimit && progress == outbound.DocRangeWorkPending; step++ {
		progress, err = recovery.Step(ctx, current)
		if err != nil {
			test.Fatal(err)
		}
	}
	if progress != outbound.DocRangeWorkPublishable || trace.bodyReads.Load() != queries {
		test.Fatal("canonical handoff recovery reread original bytes or failed its fresh proof", progress)
	}
	if err := recovery.Publish(ctx, current); err != nil {
		test.Fatal(err)
	}
	legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
}
