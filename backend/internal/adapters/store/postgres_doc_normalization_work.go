//go:build postgres

package store

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyNormalizationWork struct {
	want          domain.ContentHash
	verifier      *domain.CanonicalDocVerifier
	verifierOwned bool
	storage       *legacyNormalizationSpool
	preparation   *domain.LegacyNormalizationPreparation
	canonical     *domain.LegacyByteSourcePreparation
	data          *legacyDocSpool
	terminal      error
	complete      bool
	prepared      bool
	closed        bool
	once          sync.Once
	err           error
}

func newLegacyNormalizationWork(ctx context.Context, want domain.ContentHash, source domain.LegacyByteSource, verifier *domain.CanonicalDocVerifier) (_ *legacyNormalizationWork, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if domain.ValidateContentHash(want) != nil || verifier == nil {
		return nil, domain.ErrIntegrity
	}
	storage, err := newLegacyNormalizationSpool(ctx)
	if err != nil {
		return nil, err
	}
	work := &legacyNormalizationWork{want: domain.ContentHash(strings.Clone(string(want))), verifier: verifier, storage: storage}
	defer func() {
		if failure != nil {
			failure = errors.Join(failure, work.Close())
			if err := ctx.Err(); err != nil {
				failure = err
			}
		}
	}()
	work.preparation, err = domain.NewLegacyNormalizationPreparation(ctx, source, storage)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

func (work *legacyNormalizationWork) Step(ctx context.Context) (complete bool, failure error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.closed {
		return false, os.ErrClosed
	}
	if work.terminal != nil {
		return false, work.terminal
	}
	if work.complete {
		return true, nil
	}
	defer func() {
		if err := ctx.Err(); err != nil {
			complete, failure = false, err
		}
		if failure != nil && !errors.Is(failure, domain.ErrLegacyNormalizationBusy) {
			work.terminal = failure
		}
	}()
	if work.canonical == nil {
		if work.preparation == nil || work.storage == nil {
			return false, domain.ErrIntegrity
		}
		if !work.prepared {
			complete, err := work.preparation.Step(ctx)
			if err != nil {
				return false, err
			}
			work.prepared = complete
			return false, nil
		}
		source, err := work.preparation.Source()
		if err != nil {
			return false, err
		}
		work.storage.readOnly = true
		data, err := newLegacyDocSpool()
		if err != nil {
			return false, errors.Join(domain.ErrPreparedDocRangeObject, err)
		}
		work.data = data
		work.canonical, err = domain.NewLegacyByteSourcePreparation(ctx, work.want, source, &legacyProofSpool{legacyDocSpool: data}, work.verifier)
		return false, err
	}
	progress, err := work.canonical.Step(ctx)
	if err != nil {
		return false, err
	}
	switch progress {
	case domain.LegacyByteSourcePending:
		return false, nil
	case domain.LegacyByteSourceOriginalRequired:
		return false, domain.ErrIntegrity
	case domain.LegacyByteSourceVerified:
		if _, err := work.canonical.Result(); err != nil {
			return false, err
		}
		work.preparation.Close()
		if err := work.storage.Close(); err != nil {
			return false, err
		}
		work.storage, work.preparation = nil, nil
		work.complete = true
		return true, nil
	default:
		return false, domain.ErrIntegrity
	}
}

func (work *legacyNormalizationWork) inputConsumed() (bool, error) {
	if work == nil || work.closed {
		return false, os.ErrClosed
	}
	if work.terminal != nil {
		return false, work.terminal
	}
	if work.canonical != nil {
		return true, nil
	}
	if work.preparation == nil {
		return false, domain.ErrIntegrity
	}
	return work.preparation.InputConsumed()
}

func (work *legacyNormalizationWork) RetainedResources() (outbound.RootPublicationResources, error) {
	if work == nil || work.closed {
		return outbound.RootPublicationResources{}, os.ErrClosed
	}
	var resources outbound.RootPublicationResources
	if work.storage != nil {
		var err error
		resources, err = work.storage.RetainedResources()
		if err != nil {
			return outbound.RootPublicationResources{}, err
		}
	}
	if work.preparation != nil {
		count, err := work.preparation.RetainedBytes()
		if err != nil || count < 0 || resources.HeapBytes > math.MaxInt64-count {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrIntegrity, err)
		}
		resources.HeapBytes += count
	}
	if work.canonical != nil {
		count, err := work.canonical.RetainedBytes()
		if err != nil || count < 0 || resources.HeapBytes > math.MaxInt64-count {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrIntegrity, err)
		}
		resources.HeapBytes += count
	}
	estimate := rootPublicationHeapEstimate{bytes: resources.HeapBytes}
	estimate.allocation(1, int64(unsafe.Sizeof(*work)))
	estimate.allocation(int64(len(work.want)), 1)
	if work.verifierOwned {
		estimate.allocation(1, int64(unsafe.Sizeof(*work.verifier)))
	}
	if work.data != nil {
		info, err := work.data.file.Stat()
		if err != nil || work.data.size < 0 || work.data.size > domain.MaxFinalizedDocBytes || info.Size() != int64(work.data.size) ||
			resources.SpoolBytes > math.MaxInt64-info.Size() {
			return outbound.RootPublicationResources{}, errors.Join(domain.ErrPreparedDocRangeObject, err)
		}
		resources.SpoolBytes += info.Size()
		estimate.allocation(1, int64(unsafe.Sizeof(*work.data)+unsafe.Sizeof(legacyProofSpool{})))
		estimate.allocation(int64(len(work.data.file.Name())), 1)
	}
	if estimate.err != nil {
		return outbound.RootPublicationResources{}, estimate.err
	}
	resources.HeapBytes = estimate.bytes
	return resources, nil
}

func (work *legacyNormalizationWork) Close() error {
	if work == nil {
		return nil
	}
	work.once.Do(func() {
		work.closed = true
		if work.preparation != nil {
			work.preparation.Close()
		}
		work.err = work.storage.Close()
		if work.data != nil {
			if err := work.data.Close(); err != nil {
				work.err = errors.Join(work.err, domain.ErrPreparedDocRangeObject, err)
			}
		}
		work.storage, work.preparation, work.canonical, work.data = nil, nil, nil, nil
		work.want, work.verifier = "", nil
	})
	return work.err
}
