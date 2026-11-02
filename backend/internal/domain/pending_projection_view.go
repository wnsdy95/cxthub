package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type pendingProjectionRepository struct {
	Repo
	RemoteURLSupported    *bool `json:"remote_url_supported,omitempty"`
	GitRemoteURLSupported *bool `json:"git_remote_url_supported,omitempty"`
}

type pendingProjectionProtocol struct {
	ContextProtocol *int `json:"context_protocol"`
}

type pendingProjectionHeader struct {
	Version        int                 `json:"version"`
	RepoID         ContentHash         `json:"repo_id"`
	Projection     string              `json:"projection"`
	Revision       RepositoryRevision  `json:"revision"`
	DefaultBranch  string              `json:"default_branch"`
	Order          map[string][]string `json:"order"`
	OrphanSessions []string            `json:"orphan_sessions"`
	HoldCounts     map[string]int      `json:"hold_counts"`
}

type pendingProjectionRow[Value any] struct {
	Version int         `json:"version"`
	RepoID  ContentHash `json:"repo_id"`
	Value   Value       `json:"value"`
}

func ProjectPendingSource(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry) (PendingStateView, error) {
	_, view, err := decodeRepositoryViewSource(ctx, repo, entries)
	if err != nil {
		return PendingStateView{}, err
	}
	projected, err := ProjectPendingState(ctx, view)
	if err != nil {
		return PendingStateView{}, err
	}
	if _, err := EncodePendingProjection(ctx, repo, projected); err != nil {
		return PendingStateView{}, err
	}
	return projected, nil
}

func DecodeRepositoryViewSource(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry) (Repo, RepositoryView, error) {
	return decodeRepositoryGraphViewSource(ctx, repo, entries, decodeRepositorySourceRow)
}

func decodeRepositoryGraphViewSource(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry, decode func(context.Context, RepositoryViewSourceEntry) (any, error)) (Repo, RepositoryView, error) {
	metadata, view, err := decodeRepositoryViewSourceRows(ctx, repo, entries, decode)
	if err != nil {
		return Repo{}, RepositoryView{}, err
	}
	if metadata.RemoteURLSupported == nil || !*metadata.RemoteURLSupported || metadata.GitRemoteURLSupported == nil || !*metadata.GitRemoteURLSupported {
		return Repo{}, RepositoryView{}, fmt.Errorf("%w: repository source origin is unsupported", ErrIntegrity)
	}
	if err := ctx.Err(); err != nil {
		return Repo{}, RepositoryView{}, err
	}
	return metadata.Repo, view, nil
}

func decodeRepositoryViewSource(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry) (pendingProjectionRepository, RepositoryView, error) {
	return decodeRepositoryViewSourceRows(ctx, repo, entries, decodeRepositorySourceRow)
}

func decodeRepositoryViewSourceRows(ctx context.Context, repo ContentHash, entries []RepositoryViewSourceEntry, decode func(context.Context, RepositoryViewSourceEntry) (any, error)) (pendingProjectionRepository, RepositoryView, error) {
	if err := ctx.Err(); err != nil {
		return pendingProjectionRepository{}, RepositoryView{}, err
	}
	if err := ValidateContentHash(repo); err != nil {
		return pendingProjectionRepository{}, RepositoryView{}, err
	}
	if len(entries) > MaxPendingProjectionInputRows {
		return pendingProjectionRepository{}, RepositoryView{}, ErrRepositoryViewSourceLimit
	}
	var size int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return pendingProjectionRepository{}, RepositoryView{}, err
		}
		size += len(entry.Kind) + len(entry.Key) + len(entry.Value) + 128
		if size > MaxPendingProjectionInputBytes {
			return pendingProjectionRepository{}, RepositoryView{}, ErrRepositoryViewSourceLimit
		}
	}
	var metadata pendingProjectionRepository
	var protocol pendingProjectionProtocol
	view := RepositoryView{Snapshots: []Snapshot{}, Refs: []Ref{}, History: []HistoryEvent{}, Pending: []Pending{}, Unsync: []Unsync{}, Reflog: []RefLogEntry{}}
	var archives []SessionArchive
	type numberedReflog struct {
		id    int64
		entry RefLogEntry
	}
	var reflog []numberedReflog
	seen := make(map[[2]string]bool, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return pendingProjectionRepository{}, RepositoryView{}, err
		}
		key, err := pendingProjectionSourceKey(entry)
		if err != nil || entry.Deleted || entry.Sequence < 0 || seen[[2]string{entry.Kind, key}] {
			return pendingProjectionRepository{}, RepositoryView{}, fmt.Errorf("%w: invalid or duplicate repository source %s/%s", ErrIntegrity, entry.Kind, entry.Key)
		}
		seen[[2]string{entry.Kind, key}] = true
		value, err := decode(ctx, entry)
		if err != nil {
			return pendingProjectionRepository{}, RepositoryView{}, err
		}
		valid := true
		switch row := value.(type) {
		case *pendingProjectionRepository:
			metadata = *row
			valid = key == string(repo) && row.ID == repo && (row.ContextProtocol == 0 || row.ContextProtocol == 1) && (row.DefaultBranch == "" || ValidateBranchName(row.DefaultBranch) == nil)
		case *pendingProjectionProtocol:
			protocol = *row
			valid = key == string(repo) && row.ContextProtocol != nil && (*row.ContextProtocol == 0 || *row.ContextProtocol == 1)
		case *RepositoryRevision:
			view.Revision = *row
			valid = key == string(repo) && pendingProjectionFields(entry.Value, "graph", "pending")
		case *Snapshot:
			valid = key == string(row.ID) && pendingProjectionSnapshot(repo, *row) == nil
			if row.Parents == nil {
				row.Parents = []ContentHash{}
			}
			if len(row.GraftParents) == 0 {
				row.GraftParents = nil
			}
			if len(row.Models) == 0 {
				row.Models = nil
			}
			view.Snapshots = append(view.Snapshots, *row)
		case *Ref:
			valid = row.RepoID == repo && ValidateRef(*row) == nil && key == pendingProjectionPair(string(row.Kind), row.Name)
			view.Refs = append(view.Refs, *row)
		case *HistoryEvent:
			valid = row.RepoID == string(repo) && row.ID == key && ValidateHistoryEvent(*row) == nil
			view.History = append(view.History, *row)
		case *Pending:
			valid = key == row.SessionID && pendingProjectionPending(repo, *row) == nil
			view.Pending = append(view.Pending, *row)
		case *Unsync:
			valid = key == pendingProjectionPair(row.User, row.Branch) && pendingProjectionUnsync(repo, *row) == nil
			view.Unsync = append(view.Unsync, *row)
		case *SessionArchive:
			valid = key == string(row.Key) && pendingProjectionArchive(repo, *row) == nil
			archives = append(archives, *row)
		case *RefLogEntry:
			identifier, parseErr := strconv.ParseInt(key, 10, 64)
			valid = parseErr == nil && identifier > 0 && strconv.FormatInt(identifier, 10) == key && ValidateRefName(row.Kind, row.Name) == nil && ValidateOptionalContentHash(row.Old) == nil && ValidateOptionalContentHash(row.New) == nil && !row.CreatedAt.IsZero()
			reflog = append(reflog, numberedReflog{identifier, *row})
		}
		if !valid {
			return pendingProjectionRepository{}, RepositoryView{}, fmt.Errorf("%w: invalid repository source %s/%s", ErrIntegrity, entry.Kind, entry.Key)
		}
	}
	if !seen[[2]string{"repository", string(repo)}] || !seen[[2]string{"protocol", string(repo)}] || metadata.ContextProtocol != *protocol.ContextProtocol {
		return pendingProjectionRepository{}, RepositoryView{}, fmt.Errorf("%w: missing or inconsistent repository source protocol", ErrIntegrity)
	}
	view.DefaultBranch = metadata.DefaultBranch
	sort.Slice(view.Snapshots, func(left, right int) bool {
		if view.Snapshots[left].CreatedAt.Equal(view.Snapshots[right].CreatedAt) {
			return view.Snapshots[left].ID < view.Snapshots[right].ID
		}
		return view.Snapshots[left].CreatedAt.After(view.Snapshots[right].CreatedAt)
	})
	sort.Slice(view.Refs, func(left, right int) bool {
		if view.Refs[left].Kind == view.Refs[right].Kind {
			return view.Refs[left].Name < view.Refs[right].Name
		}
		return view.Refs[left].Kind < view.Refs[right].Kind
	})
	var err error
	if metadata.ContextProtocol == 0 {
		view.Refs, err = ProjectBranchLifecycleRefs(view.Refs)
		if err != nil {
			return pendingProjectionRepository{}, RepositoryView{}, err
		}
	}
	view.History, err = OrderHistoryEvents(view.History)
	if err != nil {
		return pendingProjectionRepository{}, RepositoryView{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if _, err := ProjectContextBranches(view.History); err != nil {
		return pendingProjectionRepository{}, RepositoryView{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	sort.Slice(reflog, func(left, right int) bool { return reflog[left].id > reflog[right].id })
	for _, entry := range reflog {
		view.Reflog = append(view.Reflog, entry.entry)
	}
	sort.Slice(view.Unsync, func(left, right int) bool {
		if !view.Unsync[left].UpdatedAt.Equal(view.Unsync[right].UpdatedAt) {
			return view.Unsync[left].UpdatedAt.After(view.Unsync[right].UpdatedAt)
		}
		if view.Unsync[left].User != view.Unsync[right].User {
			return view.Unsync[left].User < view.Unsync[right].User
		}
		return view.Unsync[left].Branch < view.Unsync[right].Branch
	})
	if err := ctx.Err(); err != nil {
		return pendingProjectionRepository{}, RepositoryView{}, err
	}
	view.ArchivedSessions = ProjectSessionArchives(archives, view.Snapshots, view.History, view.DefaultBranch)
	byID := make(map[ContentHash]Snapshot, len(view.Snapshots))
	for _, snapshot := range view.Snapshots {
		byID[snapshot.ID] = snapshot
	}
	for index := range view.Pending {
		if snapshot, exists := byID[view.Pending[index].Target]; exists && !snapshot.CreatedAt.IsZero() {
			view.Pending[index].UpdatedAt = snapshot.CreatedAt
		}
	}
	return metadata, view, nil
}

func pendingProjectionSourceKey(entry RepositoryViewSourceEntry) (string, error) {
	if !RepositoryViewSourceKindValid(entry.Kind) || entry.Key == "" || len(entry.Key) > MaxRepositoryViewSourceKeyBytes {
		return "", ErrIntegrity
	}
	if entry.Kind != "ref" && entry.Kind != "unsync" {
		return entry.Key, nil
	}
	var pair []string
	if json.Unmarshal([]byte(entry.Key), &pair) != nil || len(pair) != 2 || pair[0] == "" || pair[1] == "" {
		return "", ErrIntegrity
	}
	return pendingProjectionPair(pair[0], pair[1]), nil
}

func pendingProjectionPair(first, second string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([2]string{first, second})
	return strings.TrimSuffix(encoded.String(), "\n")
}

func DecodeRepositorySourceJSON(ctx context.Context, raw []byte, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ErrIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := pendingProjectionJSONTokens(ctx, decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: repository source JSON: %v", ErrIntegrity, err)
	}
	return ctx.Err()
}

func pendingProjectionJSONTokens(ctx context.Context, decoder *json.Decoder, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return ErrIntegrity
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrIntegrity
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrIntegrity
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			keyToken, err := decoder.Token()
			key, valid := keyToken.(string)
			if err != nil || !valid || keys[key] {
				return ErrIntegrity
			}
			keys[key] = true
		}
		if err := pendingProjectionJSONTokens(ctx, decoder, depth+1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || delimiter == '{' && closing != json.Delim('}') || delimiter == '[' && closing != json.Delim(']') {
		return ErrIntegrity
	}
	return nil
}

func pendingProjectionFields(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range names {
		value := bytes.TrimSpace(fields[name])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return false
		}
	}
	return true
}

func pendingProjectionSnapshot(repo ContentHash, snapshot Snapshot) error {
	if snapshot.RepoID != repo || ValidateContentHash(snapshot.ID) != nil || snapshot.ID != snapshot.DocHash || snapshot.DocIdentity.Validate() != nil || snapshot.GraftSeq > MaxGraftSeq || snapshot.CompactionCount < 0 {
		return ErrIntegrity
	}
	for _, hash := range []ContentHash{snapshot.MemoryHash, snapshot.ClaudeSettings, snapshot.AgentsSettings, snapshot.CodexSettings} {
		if ValidateOptionalContentHash(hash) != nil {
			return ErrIntegrity
		}
	}
	for _, parents := range [][]ContentHash{snapshot.Parents, snapshot.GraftParents} {
		for _, parent := range parents {
			if ValidateContentHash(parent) != nil {
				return ErrIntegrity
			}
		}
	}
	return nil
}

func pendingProjectionPending(repo ContentHash, pending Pending) error {
	if pending.RepoID != repo || pending.SessionID == "" || len(pending.SessionID) > 128 || ValidateContentHash(pending.Target) != nil {
		return ErrIntegrity
	}
	return nil
}

func pendingProjectionUnsync(repo ContentHash, unsync Unsync) error {
	if unsync.RepoID != repo || unsync.User == "" || len(unsync.User) > 128 || ValidateBranchName(unsync.Branch) != nil || ValidateContentHash(unsync.Target) != nil {
		return ErrIntegrity
	}
	return nil
}

func pendingProjectionArchive(repo ContentHash, archive SessionArchive) error {
	if archive.RepoID != repo || ValidateContentHash(archive.Key) != nil || ValidateContentHash(archive.SnapshotID) != nil || archive.ArchivedAt.IsZero() || strings.TrimSpace(archive.ArchivedBy) == "" || archive.Key != SessionArchiveKey(Snapshot{ID: archive.SnapshotID, Provider: archive.Provider, SessionID: archive.SessionID}) {
		return ErrIntegrity
	}
	return nil
}

func pendingProjectionRowKey(repo ContentHash, value any) (string, error) {
	switch row := value.(type) {
	case Ref:
		if row.RepoID != repo || ValidateRef(row) != nil {
			return "", ErrIntegrity
		}
		if _, _, err := ParseBranchLifecycleRef(row); err != nil {
			return "", ErrIntegrity
		}
		return pendingProjectionPair(string(row.Kind), row.Name), nil
	case Pending:
		return row.SessionID, pendingProjectionPending(repo, row)
	case Snapshot:
		if len(row.Branches) != 0 {
			return "", ErrIntegrity
		}
		return string(row.ID), pendingProjectionSnapshot(repo, row)
	case SessionArchiveView:
		if err := pendingProjectionArchive(repo, row.SessionArchive); err != nil {
			return "", err
		}
		for _, hash := range []ContentHash{row.LatestSnapshotID, row.Origin.ParentSnapshotID, row.Origin.MainSnapshotID} {
			if ValidateOptionalContentHash(hash) != nil {
				return "", ErrIntegrity
			}
		}
		seen := map[ContentHash]bool{}
		for _, hash := range row.SnapshotIDs {
			if ValidateContentHash(hash) != nil || seen[hash] {
				return "", ErrIntegrity
			}
			seen[hash] = true
		}
		if row.LatestSnapshotID != "" && !seen[row.LatestSnapshotID] || row.Origin.MainGitCommit != "" && ValidateGitOID(row.Origin.MainGitCommit) != nil {
			return "", ErrIntegrity
		}
		return string(row.Key), nil
	case GraphHoldCluster:
		if len(row.IDs) == 0 || len(row.Tips) == 0 {
			return "", ErrIntegrity
		}
		members := map[ContentHash]bool{}
		for _, hash := range row.IDs {
			if ValidateContentHash(hash) != nil || members[hash] {
				return "", ErrIntegrity
			}
			members[hash] = true
		}
		tips := map[string]bool{}
		for _, tip := range row.Tips {
			key := pendingProjectionPair(tip.User, tip.Branch)
			if pendingProjectionUnsync(repo, tip) != nil || !members[tip.Target] || tips[key] {
				return "", ErrIntegrity
			}
			tips[key] = true
		}
		ids := append([]ContentHash{}, row.IDs...)
		sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
		raw, err := json.Marshal(ids)
		return string(HashContent(raw)), err
	default:
		return "", ErrIntegrity
	}
}

type pendingProjectionWriter struct {
	ctx   context.Context
	repo  ContentHash
	parts []PendingProjectionPart
	size  int
}

func (writer *pendingProjectionWriter) add(kind, key string, value any) error {
	if err := writer.ctx.Err(); err != nil {
		return err
	}
	if key == "" || len(key) > MaxRepositoryViewSourceKeyBytes {
		return ErrIntegrity
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	writer.size += len(kind) + len(key) + len(raw) + 128
	if writer.size > MaxPendingProjectionOutputBytes || len(writer.parts) >= MaxPendingProjectionOutputRows {
		return ErrRepositoryViewSourceLimit
	}
	writer.parts = append(writer.parts, PendingProjectionPart{Kind: kind, Key: key, Value: raw})
	return writer.ctx.Err()
}

func pendingProjectionEncodeRows[Value any](writer *pendingProjectionWriter, kind string, values []Value) ([]string, error) {
	var keys []string
	if values != nil {
		keys = make([]string, 0, len(values))
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := writer.ctx.Err(); err != nil {
			return nil, err
		}
		key, err := pendingProjectionRowKey(writer.repo, value)
		if err != nil || seen[key] {
			return nil, ErrIntegrity
		}
		seen[key] = true
		if err := writer.add(kind, key, pendingProjectionRow[Value]{Version: PendingProjectionRules, RepoID: writer.repo, Value: value}); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func EncodePendingProjection(ctx context.Context, repo ContentHash, view PendingStateView) ([]PendingProjectionPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(repo) != nil {
		return nil, ErrIntegrity
	}
	if 1+len(view.Refs)+len(view.Pending)+len(view.Snapshots)+len(view.ArchivedSessions)+len(view.Hold) > MaxPendingProjectionOutputRows {
		return nil, ErrRepositoryViewSourceLimit
	}
	if err := pendingProjectionView(ctx, view); err != nil {
		return nil, err
	}
	writer := pendingProjectionWriter{ctx: ctx, repo: repo}
	header := pendingProjectionHeader{Version: PendingProjectionRules, RepoID: repo, Projection: view.Projection, Revision: view.Revision, DefaultBranch: view.DefaultBranch, Order: map[string][]string{}, OrphanSessions: view.OrphanSessions, HoldCounts: view.HoldCounts}
	var err error
	if header.Order["ref"], err = pendingProjectionEncodeRows(&writer, "ref", view.Refs); err != nil {
		return nil, err
	}
	if header.Order["pending"], err = pendingProjectionEncodeRows(&writer, "pending", view.Pending); err != nil {
		return nil, err
	}
	if header.Order["snapshot"], err = pendingProjectionEncodeRows(&writer, "snapshot", view.Snapshots); err != nil {
		return nil, err
	}
	if header.Order["archive"], err = pendingProjectionEncodeRows(&writer, "archive", view.ArchivedSessions); err != nil {
		return nil, err
	}
	if header.Order["hold"], err = pendingProjectionEncodeRows(&writer, "hold", view.Hold); err != nil {
		return nil, err
	}
	if err := writer.add("header", "view", header); err != nil {
		return nil, err
	}
	sort.Slice(writer.parts, func(left, right int) bool {
		if writer.parts[left].Kind == writer.parts[right].Kind {
			return writer.parts[left].Key < writer.parts[right].Key
		}
		return writer.parts[left].Kind < writer.parts[right].Kind
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return writer.parts, nil
}

func pendingProjectionDecodeRows[Value any](ctx context.Context, repo ContentHash, kind string, header pendingProjectionHeader, parts map[[2]string]json.RawMessage) ([]Value, error) {
	keys, exists := header.Order[kind]
	if !exists {
		return nil, ErrIntegrity
	}
	var values []Value
	if keys != nil {
		values = make([]Value, 0, len(keys))
	}
	for _, key := range keys {
		raw, exists := parts[[2]string{kind, key}]
		if !exists {
			return nil, ErrIntegrity
		}
		delete(parts, [2]string{kind, key})
		var row pendingProjectionRow[Value]
		if err := DecodeRepositorySourceJSON(ctx, raw, &row); err != nil {
			return nil, err
		}
		actualKey, err := pendingProjectionRowKey(repo, row.Value)
		if err != nil || row.Version != PendingProjectionRules || row.RepoID != repo || actualKey != key {
			return nil, ErrIntegrity
		}
		values = append(values, row.Value)
	}
	return values, nil
}

func DecodePendingProjection(ctx context.Context, repo ContentHash, parts []PendingProjectionPart) (PendingStateView, error) {
	if err := ctx.Err(); err != nil {
		return PendingStateView{}, err
	}
	if ValidateContentHash(repo) != nil {
		return PendingStateView{}, ErrIntegrity
	}
	if len(parts) > MaxPendingProjectionOutputRows {
		return PendingStateView{}, ErrRepositoryViewSourceLimit
	}
	rows := make(map[[2]string]json.RawMessage, len(parts))
	var size int
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return PendingStateView{}, err
		}
		size += len(part.Kind) + len(part.Key) + len(part.Value) + 128
		if size > MaxPendingProjectionOutputBytes {
			return PendingStateView{}, ErrRepositoryViewSourceLimit
		}
		identity := [2]string{part.Kind, part.Key}
		if _, exists := rows[identity]; exists || part.Key == "" || len(part.Key) > MaxRepositoryViewSourceKeyBytes || len(part.Kind) > 128 {
			return PendingStateView{}, ErrIntegrity
		}
		rows[identity] = part.Value
	}
	var header pendingProjectionHeader
	raw := rows[[2]string{"header", "view"}]
	if err := DecodeRepositorySourceJSON(ctx, raw, &header); err != nil {
		return PendingStateView{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || header.Version != PendingProjectionRules || header.RepoID != repo || len(header.Order) != 5 || !pendingProjectionFields(raw, "version", "repo_id", "projection", "revision", "default_branch", "order") || !pendingProjectionFields(fields["revision"], "graph", "pending") {
		return PendingStateView{}, ErrIntegrity
	}
	if _, exists := fields["orphan_sessions"]; !exists {
		return PendingStateView{}, ErrIntegrity
	}
	if _, exists := fields["hold_counts"]; !exists {
		return PendingStateView{}, ErrIntegrity
	}
	orderedRows := 1
	for _, keys := range header.Order {
		orderedRows += len(keys)
	}
	if orderedRows != len(parts) || len(header.OrphanSessions) > len(header.Order["pending"]) {
		return PendingStateView{}, ErrIntegrity
	}
	delete(rows, [2]string{"header", "view"})
	view := PendingStateView{Projection: header.Projection, Revision: header.Revision, DefaultBranch: header.DefaultBranch, OrphanSessions: header.OrphanSessions, HoldCounts: header.HoldCounts}
	var err error
	if view.Refs, err = pendingProjectionDecodeRows[Ref](ctx, repo, "ref", header, rows); err != nil {
		return PendingStateView{}, err
	}
	if view.Pending, err = pendingProjectionDecodeRows[Pending](ctx, repo, "pending", header, rows); err != nil {
		return PendingStateView{}, err
	}
	if view.Snapshots, err = pendingProjectionDecodeRows[Snapshot](ctx, repo, "snapshot", header, rows); err != nil {
		return PendingStateView{}, err
	}
	if view.ArchivedSessions, err = pendingProjectionDecodeRows[SessionArchiveView](ctx, repo, "archive", header, rows); err != nil {
		return PendingStateView{}, err
	}
	if view.Hold, err = pendingProjectionDecodeRows[GraphHoldCluster](ctx, repo, "hold", header, rows); err != nil {
		return PendingStateView{}, err
	}
	if len(rows) != 0 {
		return PendingStateView{}, ErrIntegrity
	}
	if err := pendingProjectionView(ctx, view); err != nil {
		return PendingStateView{}, err
	}
	return view, nil
}

func pendingProjectionView(ctx context.Context, view PendingStateView) error {
	if view.ReadGeneration != nil || view.Projection != "on-hold-v1" || view.DefaultBranch != "" && ValidateBranchName(view.DefaultBranch) != nil {
		return ErrIntegrity
	}
	snapshots := make(map[ContentHash]Snapshot, len(view.Snapshots))
	for _, snapshot := range view.Snapshots {
		if _, exists := snapshots[snapshot.ID]; exists {
			return ErrIntegrity
		}
		snapshots[snapshot.ID] = snapshot
	}
	if err := ValidateAcyclicAncestry(ctx, snapshots); err != nil {
		return err
	}
	pendings := make(map[string]Pending, len(view.Pending))
	for _, pending := range view.Pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := snapshots[pending.Target]; !exists {
			return ErrIntegrity
		}
		pendings[pending.SessionID] = pending
	}
	held := map[ContentHash]bool{}
	tips := map[string]bool{}
	counts := map[string]int{}
	for _, cluster := range view.Hold {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, hash := range cluster.IDs {
			if _, exists := snapshots[hash]; !exists || held[hash] {
				return ErrIntegrity
			}
			held[hash] = true
		}
		branches := map[string]bool{}
		for _, tip := range cluster.Tips {
			key := pendingProjectionPair(tip.User, tip.Branch)
			if tips[key] {
				return ErrIntegrity
			}
			tips[key] = true
			branches[tip.Branch] = true
		}
		for branch := range branches {
			counts[branch] += len(cluster.IDs)
		}
	}
	orphans := map[string]bool{}
	for _, session := range view.OrphanSessions {
		pending, exists := pendings[session]
		if !exists || pending.Dismissed || held[pending.Target] || orphans[session] {
			return ErrIntegrity
		}
		orphans[session] = true
		counts[pending.Branch]++
	}
	if len(counts) != len(view.HoldCounts) {
		return ErrIntegrity
	}
	for branch, count := range view.HoldCounts {
		if count <= 0 || counts[branch] != count {
			return ErrIntegrity
		}
	}
	return ctx.Err()
}
