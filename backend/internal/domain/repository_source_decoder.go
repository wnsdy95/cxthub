package domain

import (
	"context"
	"slices"
	"time"
)

const (
	maxRepositorySourceDecoderRows  = 4096
	maxRepositorySourceDecoderBytes = 4 << 20
)

type RepositoryViewSourceDecoder struct {
	repo ContentHash
	rows map[[2]string]repositorySourceDecodedRow
}

type repositorySourceDecodedRow struct {
	raw   string
	value any
}

func DecodeRepositoryViewSourceReusing(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry, previous *RepositoryViewSourceDecoder) (Repo, RepositoryView, *RepositoryViewSourceDecoder, error) {
	if previous != nil && previous.repo != repo {
		return Repo{}, RepositoryView{}, nil, ErrIntegrity
	}
	next := &RepositoryViewSourceDecoder{repo: repo, rows: make(map[[2]string]repositorySourceDecodedRow)}
	retainedBytes := 0
	decode := func(ctx context.Context, entry RepositoryViewSourceEntry) (any, error) {
		key := [2]string{entry.Kind, entry.Key}
		var retained repositorySourceDecodedRow
		if previous != nil {
			retained = previous.rows[key]
		}
		reuse := retained.value != nil && retained.raw == string(entry.Value)
		cost := len(entry.Kind) + len(entry.Key) + len(entry.Value) + 128
		retain := len(next.rows) < maxRepositorySourceDecoderRows && retainedBytes+cost <= maxRepositorySourceDecoderBytes
		if !reuse {
			value, err := decodeRepositorySourceRow(ctx, entry)
			if err != nil {
				return nil, err
			}
			if !retain {
				return value, nil
			}
			retained = repositorySourceDecodedRow{raw: string(entry.Value), value: value}
		}
		value, err := cloneRepositorySourceRow(retained.value)
		if err != nil {
			return nil, err
		}
		if retain {
			next.rows[key] = retained
			retainedBytes += cost
		}
		return value, ctx.Err()
	}
	metadata, view, err := decodeRepositoryGraphViewSource(ctx, repo, entries, decode)
	if err != nil {
		return Repo{}, RepositoryView{}, nil, err
	}
	return metadata, view, next, nil
}

func decodeRepositorySourceRow(ctx context.Context, entry RepositoryViewSourceEntry) (any, error) {
	var value any
	switch entry.Kind {
	case "repository":
		value = &pendingProjectionRepository{}
	case "protocol":
		value = &pendingProjectionProtocol{}
	case "revision":
		value = &RepositoryRevision{}
	case "snapshot":
		return decodeRepositorySnapshotSource(ctx, entry.Value)
	case "ref":
		value = &Ref{}
	case "history":
		value = &HistoryEvent{}
	case "pending":
		value = &Pending{}
	case "unsync":
		value = &Unsync{}
	case "session_archive":
		value = &SessionArchive{}
	case "reflog":
		value = &RefLogEntry{}
	default:
		return nil, ErrIntegrity
	}
	if err := DecodeRepositorySourceJSON(ctx, entry.Value, value); err != nil {
		return nil, err
	}
	var times []time.Time
	switch row := value.(type) {
	case *HistoryEvent:
		times = []time.Time{row.CreatedAt}
	case *Pending:
		times = []time.Time{row.UpdatedAt}
		if row.ActivityAt != nil {
			times = append(times, *row.ActivityAt)
		}
	case *Unsync:
		times = []time.Time{row.UpdatedAt}
	case *SessionArchive:
		times = []time.Time{row.ArchivedAt}
	case *RefLogEntry:
		times = []time.Time{row.CreatedAt}
	}
	for _, stamp := range times {
		if _, err := stamp.MarshalJSON(); err != nil {
			return nil, ErrIntegrity
		}
	}
	return value, nil
}

func decodeRepositorySnapshotSource(ctx context.Context, raw []byte) (*Snapshot, error) {
	type snapshotWire Snapshot
	var snapshot snapshotWire
	input := struct {
		*snapshotWire
		DocIdentity singleDocumentIdentity `json:"doc_identity"`
	}{snapshotWire: &snapshot}
	if err := DecodeRepositorySourceJSON(ctx, raw, &input); err != nil {
		return nil, err
	}
	if _, err := snapshot.CreatedAt.MarshalJSON(); err != nil {
		return nil, ErrIntegrity
	}
	snapshot.DocIdentity = input.DocIdentity.value
	return (*Snapshot)(&snapshot), nil
}

func cloneRepositorySourceRow(value any) (any, error) {
	switch row := value.(type) {
	case *pendingProjectionRepository:
		cloned := *row
		cloned.Topics = slices.Clone(row.Topics)
		if row.RemoteURLSupported != nil {
			flag := *row.RemoteURLSupported
			cloned.RemoteURLSupported = &flag
		}
		if row.GitRemoteURLSupported != nil {
			flag := *row.GitRemoteURLSupported
			cloned.GitRemoteURLSupported = &flag
		}
		return &cloned, nil
	case *pendingProjectionProtocol:
		cloned := *row
		if row.ContextProtocol != nil {
			version := *row.ContextProtocol
			cloned.ContextProtocol = &version
		}
		return &cloned, nil
	case *RepositoryRevision:
		cloned := *row
		return &cloned, nil
	case *Snapshot:
		cloned := row.Clone()
		return &cloned, nil
	case *Ref:
		cloned := *row
		return &cloned, nil
	case *HistoryEvent:
		cloned := row.Clone()
		return &cloned, nil
	case *Pending:
		cloned := *row
		if row.ActivityAt != nil {
			activity := *row.ActivityAt
			cloned.ActivityAt = &activity
		}
		return &cloned, nil
	case *Unsync:
		cloned := *row
		return &cloned, nil
	case *SessionArchive:
		cloned := *row
		return &cloned, nil
	case *RefLogEntry:
		cloned := *row
		return &cloned, nil
	default:
		return nil, ErrIntegrity
	}
}
