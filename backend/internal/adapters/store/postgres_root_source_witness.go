//go:build postgres

package store

import (
	"errors"
	"math"
	"sort"
	"unsafe"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const pgRootSourceRuleVersion uint32 = 2

type pgRootDependencySource struct {
	hash        domain.ContentHash
	version     int64
	incarnation string
}

type pgRootSourceWitness struct {
	repo       domain.ContentHash
	repoEpoch  string
	identity   domain.DocumentIdentity
	document   domain.ContentHash
	job        string
	jobVersion int64
	manifest   domain.ContentHash
	rule       uint32
	chunks     []pgRootDependencySource
}

func newPGRootSourceWitness(prepared *preparedStreamingRootDoc, sources map[domain.ContentHash]pgRootDependencySource) (pgRootSourceWitness, error) {
	if prepared == nil {
		return pgRootSourceWitness{}, domain.ErrIntegrity
	}
	if err := prepared.matches(prepared.job); err != nil {
		return pgRootSourceWitness{}, err
	}
	witness := pgRootSourceWitness{repo: prepared.job.RepoID, repoEpoch: prepared.job.RepositoryEpoch, identity: prepared.job.DocIdentity,
		document: prepared.job.DocHash, job: prepared.job.ID, jobVersion: prepared.job.Version,
		manifest: domain.HashContent(prepared.root.canonical), rule: pgRootSourceRuleVersion}
	unique := make(map[domain.ContentHash]bool, len(prepared.root.manifest.Chunks))
	for _, chunk := range prepared.root.manifest.Chunks {
		unique[chunk.Hash] = true
	}
	if len(unique) != len(sources) {
		return pgRootSourceWitness{}, domain.ErrIntegrity
	}
	witness.chunks = make([]pgRootDependencySource, 0, len(unique))
	for hash := range unique {
		source, exists := sources[hash]
		var incarnation pgtype.UUID
		if !exists || source.hash != hash || source.version < 0 || len(source.incarnation) != 36 ||
			incarnation.Scan(source.incarnation) != nil || !incarnation.Valid || incarnation.Bytes == ([16]byte{}) {
			return pgRootSourceWitness{}, domain.ErrIntegrity
		}
		witness.chunks = append(witness.chunks, source)
	}
	sort.Slice(witness.chunks, func(left, right int) bool { return witness.chunks[left].hash < witness.chunks[right].hash })
	return witness, nil
}

func (witness *pgRootSourceWitness) matches(job domain.DocFinalizationJob, canonical []byte) error {
	if witness == nil || witness.rule != pgRootSourceRuleVersion {
		return domain.ErrIntegrity
	}
	if witness.repo != job.RepoID || witness.repoEpoch != job.RepositoryEpoch || witness.identity != job.DocIdentity || witness.document != job.DocHash || witness.job != job.ID || witness.jobVersion != job.Version {
		return domain.ErrConflict
	}
	if witness.manifest != domain.HashContent(canonical) {
		return domain.ErrIntegrity
	}
	return nil
}

func (witness *pgRootSourceWitness) dependency(hash domain.ContentHash) (pgRootDependencySource, bool) {
	position := sort.Search(len(witness.chunks), func(position int) bool { return witness.chunks[position].hash >= hash })
	if position == len(witness.chunks) || witness.chunks[position].hash != hash {
		return pgRootDependencySource{}, false
	}
	return witness.chunks[position], true
}

func (publication *pgStreamingRootPublication) matches(job domain.DocFinalizationJob) error {
	if publication == nil || publication.preparedStreamingRootDoc == nil {
		return domain.ErrIntegrity
	}
	if err := publication.preparedStreamingRootDoc.matches(job); err != nil {
		return err
	}
	if err := publication.source.matches(job, publication.root.canonical); err != nil {
		return err
	}
	return publication.indexes.matches(job.DocHash, len(publication.spool.offsets)-1)
}

func (publication *pgStreamingRootPublication) Close() error {
	if publication == nil || publication.preparedStreamingRootDoc == nil {
		return domain.ErrIntegrity
	}
	publication.closeOnce.Do(func() {
		var indexesError error
		if publication.indexes != nil {
			indexesError = publication.indexes.Close()
		}
		publication.closeErr = errors.Join(publication.preparedStreamingRootDoc.Close(), indexesError)
		publication.source = pgRootSourceWitness{}
	})
	return publication.closeErr
}

func (publication *pgStreamingRootPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	if publication == nil || publication.preparedStreamingRootDoc == nil {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	if err := publication.source.matches(publication.job, publication.root.canonical); err != nil {
		return outbound.RootPublicationResources{}, err
	}
	resources, err := publication.preparedStreamingRootDoc.RetainedResources()
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	if err := publication.indexes.matches(publication.job.DocHash, len(publication.spool.offsets)-1); err != nil {
		return outbound.RootPublicationResources{}, err
	}
	estimate := rootPublicationHeapEstimate{bytes: resources.HeapBytes}
	estimate.allocation(1, int64(unsafe.Sizeof(*publication)))
	estimate.allocation(int64(cap(publication.source.chunks)), int64(unsafe.Sizeof(pgRootDependencySource{})))
	for _, value := range []string{string(publication.source.repo), publication.source.repoEpoch, string(publication.source.identity), string(publication.source.document), publication.source.job, string(publication.source.manifest)} {
		estimate.allocation(int64(len(value)), 1)
	}
	for _, source := range publication.source.chunks[:cap(publication.source.chunks)] {
		estimate.allocation(int64(len(source.hash)), 1)
		estimate.allocation(int64(len(source.incarnation)), 1)
	}
	spoolBytes, err := publication.indexes.chargeRetained(&estimate)
	if err != nil {
		return outbound.RootPublicationResources{}, err
	}
	if spoolBytes < 0 || resources.SpoolBytes > math.MaxInt64-spoolBytes {
		return outbound.RootPublicationResources{}, domain.ErrIntegrity
	}
	resources.SpoolBytes += spoolBytes
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	resources.HeapBytes = estimate.bytes
	return resources, nil
}

var _ outbound.BoundedRootDocPublication = (*pgStreamingRootPublication)(nil)
