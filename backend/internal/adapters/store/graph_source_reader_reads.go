package store

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (reader *GraphSourceReader) scope(ctx context.Context, repo domain.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateContentHash(repo) != nil || reader == nil {
		return domain.ErrValidation
	}
	if repo != reader.metadata.ID {
		return domain.ErrNotFound
	}
	return nil
}

func graphSourceRead[Value any](ctx context.Context, reader *GraphSourceReader, repo domain.ContentHash, kind, key string) (Value, error) {
	var value Value
	if err := reader.scope(ctx, repo); err != nil {
		return value, err
	}
	raw, found := reader.rows[kind][key]
	if !found {
		return value, domain.ErrNotFound
	}
	if json.Unmarshal(raw, &value) != nil {
		return value, domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		var empty Value
		return empty, err
	}
	return value, nil
}

func graphSourceList[Value any](ctx context.Context, reader *GraphSourceReader, repo domain.ContentHash, kind string) ([]Value, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return nil, err
	}
	return graphSourceReadKeys[Value](ctx, reader, repo, kind, reader.keys[kind])
}

func graphSourceReadKeys[Value any](ctx context.Context, reader *GraphSourceReader, repo domain.ContentHash, kind string, keys []string) ([]Value, error) {
	result := []Value{}
	for _, key := range keys {
		value, err := graphSourceRead[Value](ctx, reader, repo, kind, key)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (reader *GraphSourceReader) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.Repo{}, err
	}
	metadata := reader.metadata
	metadata.Topics = slices.Clone(metadata.Topics)
	if err := ctx.Err(); err != nil {
		return domain.Repo{}, err
	}
	return metadata, nil
}

func (reader *GraphSourceReader) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitCommitTree{}, err
	}
	if !graphSourceOriginValid(origin) || domain.ValidateGitOID(commit) != nil {
		return domain.GitCommitTree{}, domain.ErrValidation
	}
	return graphSourceRead[domain.GitCommitTree](ctx, reader, repo, "git_commit", graphSourceTuple(origin, commit))
}

func (reader *GraphSourceReader) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, oid string) (domain.GitTreeNode, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitTreeNode{}, err
	}
	if !graphSourceOriginValid(origin) || domain.ValidateGitOID(oid) != nil {
		return domain.GitTreeNode{}, domain.ErrValidation
	}
	return graphSourceRead[domain.GitTreeNode](ctx, reader, repo, "git_tree", graphSourceTuple(origin, oid))
}

func (reader *GraphSourceReader) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitCommitDelta{}, err
	}
	if !graphSourceOriginValid(origin) || domain.ValidateGitOID(commit) != nil || (parent != "" && (domain.ValidateGitOID(parent) != nil || len(parent) != len(commit))) {
		return domain.GitCommitDelta{}, domain.ErrValidation
	}
	key := graphSourceTuple(origin, commit, parent)
	if parent == "" {
		candidates := reader.deltaKeys[[2]string{origin, commit}]
		if len(candidates) != 1 {
			return domain.GitCommitDelta{}, domain.ErrNotFound
		}
		key = candidates[0]
	}
	record, err := graphSourceRead[domain.GitDeltaRecord](ctx, reader, repo, "git_delta", key)
	return record.Delta, err
}

func (reader *GraphSourceReader) GetPRHistoryCheckpoint(ctx context.Context, repo domain.ContentHash, origin string, providerRepositoryID int64, number int) (domain.PRHistoryCheckpoint, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.PRHistoryCheckpoint{}, err
	}
	if validatePRHistoryScope(repo, origin, number) != nil || !graphSourceOriginValid(origin) || providerRepositoryID <= 0 {
		return domain.PRHistoryCheckpoint{}, domain.ErrValidation
	}
	return graphSourceRead[domain.PRHistoryCheckpoint](ctx, reader, repo, "pr_checkpoint", graphSourceTuple(origin, strconv.FormatInt(providerRepositoryID, 10), strconv.Itoa(number)))
}

func (reader *GraphSourceReader) GetPRRefObservation(ctx context.Context, repo domain.ContentHash, origin, id string) (domain.GitRefObservation, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitRefObservation{}, err
	}
	if validatePRRefObservationLookup(repo, origin, id) != nil || !graphSourceOriginValid(origin) {
		return domain.GitRefObservation{}, domain.ErrValidation
	}
	observation, err := graphSourceRead[domain.GitRefObservation](ctx, reader, repo, "git_observation", id)
	if err != nil {
		return domain.GitRefObservation{}, err
	}
	if err := validatePRRefObservationIdentity(observation, repo, origin, id); err != nil {
		return domain.GitRefObservation{}, err
	}
	return observation, nil
}

func (reader *GraphSourceReader) ListPRParticipations(ctx context.Context, repo domain.ContentHash, origin string, number int) ([]domain.PRParticipation, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return nil, err
	}
	if validatePRHistoryScope(repo, origin, number) != nil || !graphSourceOriginValid(origin) {
		return nil, domain.ErrValidation
	}
	return graphSourceReadKeys[domain.PRParticipation](ctx, reader, repo, "pr_participation", reader.participationKeys[[2]string{origin, strconv.Itoa(number)}])
}

func (reader *GraphSourceReader) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	return graphSourceList[domain.PRContribution](ctx, reader, repo, "pr_contribution")
}

func (reader *GraphSourceReader) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	return graphSourceList[domain.GitContribution](ctx, reader, repo, "git_contribution")
}

func (reader *GraphSourceReader) GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitHeadReceipt{}, err
	}
	if validateGitHeadReceiptScope(repo, origin, version) != nil || !graphSourceOriginValid(origin) || version == 0 {
		return domain.GitHeadReceipt{}, domain.ErrValidation
	}
	return graphSourceRead[domain.GitHeadReceipt](ctx, reader, repo, "git_receipt", graphSourceTuple(origin, strconv.FormatInt(version, 10)))
}

func (reader *GraphSourceReader) GetGitContributionAdmission(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContributionAdmission, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitContributionAdmission{}, err
	}
	if domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.GitContributionAdmission{}, domain.ErrValidation
	}
	return graphSourceRead[domain.GitContributionAdmission](ctx, reader, repo, "git_admission", id)
}

func (reader *GraphSourceReader) GetGitContextHead(ctx context.Context, repo domain.ContentHash, id string) (domain.GitContextHead, error) {
	if err := reader.scope(ctx, repo); err != nil {
		return domain.GitContextHead{}, err
	}
	if domain.ValidateContentHash(domain.ContentHash("sha256:"+id)) != nil {
		return domain.GitContextHead{}, domain.ErrValidation
	}
	return graphSourceRead[domain.GitContextHead](ctx, reader, repo, "git_context_head", id)
}

func (reader *GraphSourceReader) ListGitContributionProofs(ctx context.Context, repo domain.ContentHash) ([]domain.GitContributionProof, error) {
	return graphSourceList[domain.GitContributionProof](ctx, reader, repo, "git_contribution_proof")
}
