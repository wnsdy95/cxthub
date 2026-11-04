package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphEvidenceReader = (*GraphSourceReader)(nil)

type GraphSourceReader struct {
	metadata          domain.Repo
	rows              map[string]map[string]json.RawMessage
	keys              map[string][]string
	receiptKeys       map[string]string
	deltaKeys         map[[2]string][]string
	participationKeys map[[2]string][]string
}

type graphSourcePayload[Value any] struct {
	RepoID  domain.ContentHash `json:"repo_id"`
	Payload Value              `json:"payload"`
}

type graphSourceIdentity[Value any] struct {
	graphSourcePayload[Value]
	ID string `json:"id"`
}

func NewGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, metadata domain.Repo, proofEntries []domain.RepositoryViewSourceEntry) (*GraphSourceReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	if checkpoint.Scope != domain.RepositoryGraphSourceScope {
		return nil, domain.ErrCatalogResetRequired
	}
	if len(proofEntries) > domain.MaxPendingProjectionInputRows {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	if metadata.ID != checkpoint.RepoID || metadata.LocalPath != "" || metadata.RequiredDocIdentity.Validate() != nil || (metadata.ContextProtocol != 0 && metadata.ContextProtocol != 1) || (metadata.DefaultBranch != "" && domain.ValidateBranchName(metadata.DefaultBranch) != nil) {
		return nil, domain.ErrIntegrity
	}
	for _, origin := range []string{metadata.RemoteURL, metadata.GitRemoteURL} {
		if origin != "" && !graphSourceOriginValid(origin) {
			return nil, domain.ErrIntegrity
		}
	}
	metadataSize := len(metadata.RemoteURL) + len(metadata.GitRemoteURL) + len(metadata.DefaultBranch) + len(metadata.RepositoryID) + len(metadata.Description) + len(metadata.Website)
	if len(metadata.Topics) > domain.MaxPendingProjectionInputRows {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	for _, topic := range metadata.Topics {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		metadataSize += len(topic) + 16
	}
	if metadataSize > domain.MaxPendingProjectionInputBytes {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	metadataRaw, err := json.Marshal(metadata)
	if err != nil {
		return nil, domain.ErrIntegrity
	}
	size := len(metadataRaw)
	for _, entry := range proofEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		size += len(entry.Kind) + len(entry.Key) + len(entry.Value) + 128
		if size > domain.MaxPendingProjectionInputBytes || len(entry.Value) > domain.MaxRepositoryViewSourceImageBytes || len(entry.Key) > domain.MaxRepositoryViewSourceKeyBytes {
			return nil, domain.ErrRepositoryViewSourceLimit
		}
	}
	if size > domain.MaxPendingProjectionInputBytes {
		return nil, domain.ErrRepositoryViewSourceLimit
	}
	metadata.Topics = slices.Clone(metadata.Topics)
	reader := &GraphSourceReader{metadata: metadata, rows: make(map[string]map[string]json.RawMessage), keys: make(map[string][]string), receiptKeys: make(map[string]string),
		deltaKeys: make(map[[2]string][]string), participationKeys: make(map[[2]string][]string)}
	unique := make(map[[2]string]bool)
	for _, entry := range proofEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Deleted || entry.Sequence < 0 || entry.Sequence > checkpoint.Sequence || entry.Key == "" {
			return nil, domain.ErrIntegrity
		}
		if err := reader.add(ctx, entry, unique); err != nil {
			return nil, fmt.Errorf("%w: graph source %s", err, entry.Kind)
		}
	}
	for kind := range reader.rows {
		for key := range reader.rows[kind] {
			reader.keys[kind] = append(reader.keys[kind], key)
		}
		slices.Sort(reader.keys[kind])
	}
	for _, keys := range reader.participationKeys {
		slices.Sort(keys)
	}
	if err := reader.validateJoins(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return reader, nil
}

func graphSourceOriginValid(origin string) bool {
	if origin == "" || len(origin) > domain.MaxRepositoryViewSourceKeyBytes || strings.ContainsAny(origin, "?#\\") || strings.IndexFunc(origin, unicode.IsControl) != -1 || domain.SanitizeRemoteURL(origin) != origin {
		return false
	}
	if strings.Contains(origin, "://") {
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Hostname() != ""
	}
	return true
}

func graphSourceTuple(parts ...string) string {
	raw, _ := json.Marshal(parts)
	return string(raw)
}

func graphSourceDecode[Row any](ctx context.Context, entry domain.RepositoryViewSourceEntry, fields string) (Row, json.RawMessage, error) {
	var row Row
	if err := domain.DecodeRepositorySourceJSON(ctx, entry.Value, &row); err != nil {
		return row, nil, err
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(entry.Value, &envelope) != nil {
		return row, nil, domain.ErrIntegrity
	}
	names := strings.Fields("repo_id payload " + fields)
	if len(envelope) != len(names) {
		return row, nil, domain.ErrIntegrity
	}
	for _, name := range names {
		value := bytes.TrimSpace(envelope[name])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return row, nil, domain.ErrIntegrity
		}
	}
	return row, envelope["payload"], ctx.Err()
}

func graphSourceUnique(unique map[[2]string]bool, kind, key string) error {
	identity := [2]string{kind, key}
	if unique[identity] {
		return domain.ErrIntegrity
	}
	unique[identity] = true
	return nil
}

func (reader *GraphSourceReader) add(ctx context.Context, entry domain.RepositoryViewSourceEntry, unique map[[2]string]bool) error {
	var payload json.RawMessage
	var record interface{ Validate() error }
	var repo domain.ContentHash
	var origin, key, id string
	var tuple bool
	var lookup [2]string
	switch entry.Kind {
	case "git_commit":
		row, raw, err := graphSourceDecode[struct {
			graphSourcePayload[domain.GitCommitTree]
			Origin string `json:"origin"`
			Commit string `json:"commit_oid"`
		}](ctx, entry, "origin commit_oid")
		if err != nil {
			return err
		}
		if row.Payload.Commit != row.Commit {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin = raw, row.Payload, row.RepoID, row.Origin
		key, tuple = graphSourceTuple(origin, row.Commit), true
	case "git_tree":
		row, raw, err := graphSourceDecode[struct {
			graphSourcePayload[domain.GitTreeNode]
			Origin string `json:"origin"`
			OID    string `json:"oid"`
		}](ctx, entry, "origin oid")
		if err != nil {
			return err
		}
		if row.Payload.OID != row.OID {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin = raw, row.Payload, row.RepoID, row.Origin
		key, tuple = graphSourceTuple(origin, row.OID), true
	case "git_delta":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.GitDeltaRecord]
			Origin string `json:"origin"`
			Commit string `json:"commit_oid"`
			Parent string `json:"parent_oid"`
		}](ctx, entry, "id origin commit_oid parent_oid")
		if err != nil {
			return err
		}
		value := row.Payload
		if value.ID != row.ID || value.RepoID != row.RepoID || value.GitOrigin != row.Origin || value.Delta.Commit != row.Commit || value.Delta.Parent != row.Parent {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, id = raw, value, row.RepoID, row.Origin, row.ID
		key, tuple = graphSourceTuple(origin, row.Commit, row.Parent), true
		lookup = [2]string{origin, row.Commit}
	case "git_observation":
		row, raw, err := graphSourceDecode[graphSourceIdentity[domain.GitRefObservation]](ctx, entry, "id")
		if err != nil {
			return err
		}
		if validatePRRefObservationIdentity(row.Payload, row.RepoID, row.Payload.GitOrigin, row.ID) != nil {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Payload.GitOrigin, row.ID
	case "pr_checkpoint":
		row, raw, err := graphSourceDecode[struct {
			graphSourcePayload[domain.PRHistoryCheckpoint]
			Origin               string `json:"origin"`
			ProviderRepositoryID int64  `json:"provider_repository_id"`
			Number               int    `json:"number"`
			Version              int64  `json:"version"`
		}](ctx, entry, "origin provider_repository_id number version")
		if err != nil {
			return err
		}
		if validatePRCheckpointScope(row.Payload, row.RepoID, row.Origin, row.ProviderRepositoryID, row.Number) != nil || row.Payload.Version != row.Version {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin = raw, row.Payload, row.RepoID, row.Origin
		key, tuple = graphSourceTuple(origin, strconv.FormatInt(row.ProviderRepositoryID, 10), strconv.Itoa(row.Number)), true
	case "pr_participation":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.PRParticipation]
			Origin               string `json:"origin"`
			ProviderRepositoryID int64  `json:"provider_repository_id"`
			Number               int    `json:"number"`
		}](ctx, entry, "id origin provider_repository_id number")
		if err != nil {
			return err
		}
		value := row.Payload
		if value.ID != row.ID || value.RepoID != row.RepoID || value.GitOrigin != row.Origin || value.ProviderRepositoryID != row.ProviderRepositoryID || value.Number != row.Number {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, key = raw, value, row.RepoID, row.Origin, row.ID
		lookup = [2]string{origin, strconv.Itoa(row.Number)}
	case "pr_contribution":
		row, raw, err := graphSourceDecode[graphSourceIdentity[domain.PRContribution]](ctx, entry, "id")
		if err != nil {
			return err
		}
		if row.Payload.ID != row.ID || row.Payload.RepoID != row.RepoID {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Payload.GitOrigin, row.ID
	case "git_contribution":
		row, raw, err := graphSourceDecode[graphSourceIdentity[domain.GitContribution]](ctx, entry, "id")
		if err != nil {
			return err
		}
		if row.Payload.ID != row.ID || row.Payload.RepoID != row.RepoID {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Payload.GitOrigin, row.ID
	case "git_receipt":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.GitHeadReceipt]
			Origin  string `json:"origin"`
			Version int64  `json:"version"`
		}](ctx, entry, "id origin version")
		if err != nil {
			return err
		}
		if validateGitHeadReceiptIdentity(row.Payload, row.RepoID, row.Origin, row.Version) != nil || row.Payload.ID != row.ID {
			return domain.ErrIntegrity
		}
		payload, record, repo, origin, id = raw, row.Payload, row.RepoID, row.Origin, row.ID
		key, tuple = graphSourceTuple(origin, strconv.FormatInt(row.Version, 10)), true
		reader.receiptKeys[id] = key
	case "git_admission":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.GitContributionAdmission]
			Key       string `json:"admission_key"`
			Origin    string `json:"origin"`
			ReceiptID string `json:"receipt_id"`
		}](ctx, entry, "id admission_key origin receipt_id")
		if err != nil {
			return err
		}
		if validateGitContributionAdmissionIdentity(row.Payload, row.RepoID, row.ID, row.Key, row.Origin, row.ReceiptID) != nil {
			return domain.ErrIntegrity
		}
		if err := graphSourceUnique(unique, "admission_key", row.Key); err != nil {
			return err
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Origin, row.ID
	case "git_context_head":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.GitContextHead]
			Origin    string `json:"origin"`
			BranchID  string `json:"destination_branch_id"`
			Branch    string `json:"destination_branch"`
			Version   int64  `json:"read_version"`
			ReceiptID string `json:"receipt_id"`
		}](ctx, entry, "id origin destination_branch_id destination_branch read_version receipt_id")
		if err != nil {
			return err
		}
		if validateGitContextHeadIdentity(row.Payload, row.RepoID, row.ID, row.Origin, row.BranchID, row.Branch, row.Version, row.ReceiptID) != nil {
			return domain.ErrIntegrity
		}
		for name, branch := range map[string]string{"context_branch_id": row.BranchID, "context_branch": row.Branch} {
			if err := graphSourceUnique(unique, name, graphSourceTuple(row.Origin, strconv.FormatInt(row.Version, 10), branch)); err != nil {
				return err
			}
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Origin, row.ID
	case "git_contribution_proof":
		row, raw, err := graphSourceDecode[struct {
			graphSourceIdentity[domain.GitContributionProof]
			AdmissionID    string `json:"admission_id"`
			ContextHeadID  string `json:"context_head_id"`
			ContributionID string `json:"contribution_id"`
		}](ctx, entry, "id admission_id context_head_id contribution_id")
		if err != nil {
			return err
		}
		if validateGitContributionProofIdentity(row.Payload, row.RepoID, row.ID, row.AdmissionID, row.ContextHeadID, row.ContributionID) != nil {
			return domain.ErrIntegrity
		}
		if err := graphSourceUnique(unique, "proof_admission", row.AdmissionID); err != nil {
			return err
		}
		payload, record, repo, origin, key = raw, row.Payload, row.RepoID, row.Payload.Contribution.GitOrigin, row.ID
	default:
		return domain.ErrIntegrity
	}
	if repo != reader.metadata.ID || !graphSourceOriginValid(origin) || record.Validate() != nil {
		return domain.ErrIntegrity
	}
	entryKey := entry.Key
	if tuple {
		var parts []string
		if json.Unmarshal([]byte(entryKey), &parts) != nil {
			return domain.ErrIntegrity
		}
		entryKey = graphSourceTuple(parts...)
	}
	if entryKey != key {
		return domain.ErrIntegrity
	}
	if id != "" {
		if err := graphSourceUnique(unique, entry.Kind, id); err != nil {
			return err
		}
	}
	if reader.rows[entry.Kind] == nil {
		reader.rows[entry.Kind] = make(map[string]json.RawMessage)
	}
	if _, exists := reader.rows[entry.Kind][key]; exists {
		return domain.ErrIntegrity
	}
	reader.rows[entry.Kind][key] = payload
	switch entry.Kind {
	case "git_delta":
		reader.deltaKeys[lookup] = append(reader.deltaKeys[lookup], key)
	case "pr_participation":
		reader.participationKeys[lookup] = append(reader.participationKeys[lookup], key)
	}
	return ctx.Err()
}

func (reader *GraphSourceReader) validateJoins(ctx context.Context) error {
	repo := reader.metadata.ID
	for _, key := range reader.keys["git_context_head"] {
		head, err := graphSourceRead[domain.GitContextHead](ctx, reader, repo, "git_context_head", key)
		if err != nil {
			return err
		}
		receipt, err := graphSourceRead[domain.GitHeadReceipt](ctx, reader, repo, "git_receipt", reader.receiptKeys[head.Destination.ReadReceiptID])
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		if err := validateGitContextHeadReceipt(head, receipt, repo, receipt.GitOrigin, receipt.Version, head.Destination.ReadReceiptID); err != nil {
			return err
		}
	}
	for _, key := range reader.keys["git_contribution_proof"] {
		proof, err := graphSourceRead[domain.GitContributionProof](ctx, reader, repo, "git_contribution_proof", key)
		if err != nil {
			return err
		}
		admission, err := reader.GetGitContributionAdmission(ctx, repo, proof.AdmissionID)
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		baseline, err := graphSourceRead[domain.GitHeadReceipt](ctx, reader, repo, "git_receipt", reader.receiptKeys[admission.Destination.ReadReceiptID])
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		if err := validateGitContributionProofBaseline(admission, baseline, repo, baseline.GitOrigin, baseline.Version, admission.Destination.ReadReceiptID); err != nil {
			return err
		}
		head, err := reader.GetGitContextHead(ctx, repo, proof.ContextHeadID)
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		receipt, err := graphSourceRead[domain.GitHeadReceipt](ctx, reader, repo, "git_receipt", reader.receiptKeys[head.Destination.ReadReceiptID])
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		contribution, err := graphSourceRead[domain.GitContribution](ctx, reader, repo, "git_contribution", proof.Contribution.ID)
		if err != nil {
			return graphSourceJoinError(ctx)
		}
		if err := validateGitContributionProofInputs(proof, admission, head, receipt, contribution); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func graphSourceJoinError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return domain.ErrIntegrity
}
