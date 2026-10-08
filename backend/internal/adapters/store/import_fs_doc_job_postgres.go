//go:build postgres

package store

import (
	"bytes"
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func verifyFrozenDocJob(ctx context.Context, source *frozenFS, job domain.DocFinalizationJob, now time.Time) (domain.DocFinalizationJob, error) {
	if err := ctx.Err(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	if err := job.Validate(); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	representation, err := job.Representation()
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	if representation.Identity == domain.DocumentIdentityLegacy {
		return job, nil
	}
	manifest, err := representation.ConversationManifest()
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	prefix := "repos/" + hexOf(job.RepoID) + "/objects/"
	doc, err := verifyStoredConversation(ctx, job.DocHash, manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return source.read(prefix + "chunks/" + hexOf(hash))
	})
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	if doc.DocumentRef() != job.DocumentRef() {
		return domain.DocFinalizationJob{}, domain.ErrIntegrity
	}
	path := prefix + "docs/" + hexOf(job.DocHash)
	_, present := source.files[path]
	if job.State == "completed" && !present {
		return domain.DocFinalizationJob{}, domain.ErrIntegrity
	}
	if present {
		raw, err := source.read(path)
		if err != nil {
			return domain.DocFinalizationJob{}, err
		}
		stored, root, err := storedConversationManifest(ctx, raw)
		if err != nil {
			return domain.DocFinalizationJob{}, err
		}
		if !root {
			return domain.DocFinalizationJob{}, domain.ErrIntegrity
		}
		canonical, err := domain.CanonicalConversationManifest(stored)
		if err != nil || !bytes.Equal(canonical, job.RootManifest) {
			return domain.DocFinalizationJob{}, domain.ErrIntegrity
		}
	}
	// Imported root workers cannot reuse a claim from the frozen FS runtime.
	// Keep legacy migration behavior unchanged; completed receipts remain metadata.
	if job.State == "running" {
		job.State = "retrying"
		job.Version++
		job.LeaseUntil = time.Time{}
		job.NextAttempt = now
		job.UpdatedAt = now
	}
	return job, job.Validate()
}
