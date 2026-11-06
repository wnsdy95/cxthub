package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func pendingStateResponseFixture(t *testing.T) (domain.ContentHash, domain.PendingStateView) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name()))
	stamp := time.Unix(123, 0).UTC()
	snapshot := func(name string, parents ...domain.ContentHash) domain.Snapshot {
		identifier := domain.HashContent([]byte(name))
		return domain.Snapshot{ID: identifier, RepoID: repo, DocHash: identifier, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionID: name, Branch: "topic", Parents: append([]domain.ContentHash{}, parents...), Message: name, CreatedAt: stamp}
	}
	root := snapshot("root")
	session := snapshot("published session", root.ID)
	archived := snapshot("archived", root.ID)
	held := snapshot("held", root.ID)
	live := snapshot("hook: live", held.ID)
	lifecycle, err := domain.NewBranchLifecycleRef(repo, "archived", archived.ID, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	source := domain.RepositoryView{
		Revision: domain.RepositoryRevision{Graph: 7, Pending: 11, Evidence: 13}, DefaultBranch: "main",
		Snapshots: []domain.Snapshot{root, session, archived, held, live},
		Refs: []domain.Ref{
			{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: root.ID},
			{RepoID: repo, Kind: domain.RefBranch, Name: "topic/\ud55c\uae00", BranchID: "topic-id", Target: root.ID},
			{RepoID: repo, Kind: domain.RefHead, Name: "HEAD", Symbolic: "main"},
			{RepoID: repo, Kind: domain.RefSession, Name: "internal/session", Target: session.ID},
			{RepoID: repo, Kind: domain.RefTag, Name: "bookmark", Target: root.ID}, lifecycle,
		},
		Unsync: []domain.Unsync{{RepoID: repo, User: "author", Branch: "topic", Target: held.ID, UpdatedAt: stamp}},
	}
	for _, capture := range []domain.Snapshot{session, archived, live} {
		source.Pending = append(source.Pending, domain.Pending{RepoID: repo, SessionID: capture.SessionID, Branch: capture.Branch, Provider: capture.Provider, Target: capture.ID, UpdatedAt: stamp})
	}
	archive := domain.SessionArchive{RepoID: repo, Key: domain.SessionArchiveKey(archived), SnapshotID: archived.ID, Provider: archived.Provider, SessionID: archived.SessionID, ArchivedAt: stamp, ArchivedBy: "maintainer"}
	source.ArchivedSessions = domain.ProjectSessionArchives([]domain.SessionArchive{archive}, source.Snapshots, nil, source.DefaultBranch)
	view, err := domain.ProjectPendingState(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Refs) != len(source.Refs) || !reflect.DeepEqual(view.OrphanSessions, []string{live.SessionID}) || len(view.Hold) != 1 || len(view.ArchivedSessions) != 1 {
		t.Fatal("fixture must retain internal publication refs, hold and archives")
	}
	return repo, view
}

func pendingStateResponseJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestPendingStateResponseKeepsOnlyBranchRefs(t *testing.T) {
	repo, view := pendingStateResponseFixture(t)
	for len(view.Refs) < 5617 {
		view.Refs = append(view.Refs, domain.Ref{RepoID: repo, Kind: domain.RefSession, Name: fmt.Sprintf("internal/session/%04d", len(view.Refs)), Target: view.Refs[0].Target})
	}
	view.ReadGeneration = &domain.PendingReadGeneration{Generation: domain.HashContent([]byte("generation")), SourceEpoch: "12345678-abcd-4321-9876-123456789abc:42", AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:42", Rules: 1, Complete: true}
	before := pendingStateResponseJSON(t, view)
	response := newPendingStateResponse(view)
	want := view
	want.Refs = append([]domain.Ref{}, view.Refs[:2]...)
	encoded := pendingStateResponseJSON(t, response)
	var got domain.PendingStateView
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("response must preserve branch metadata, classification, archives and read generation without internal refs")
	}
	if len(encoded) >= len(before)/10 {
		t.Fatalf("internal refs still dominate response: before=%d after=%d", len(before), len(encoded))
	}
	response.Refs[0].Name = "response-only"
	if !bytes.Equal(before, pendingStateResponseJSON(t, view)) {
		t.Fatal("response copy or serialization mutated the complete domain projection")
	}
	t.Logf("synthetic response: refs %d -> %d; bytes %d -> %d", len(view.Refs), len(got.Refs), len(before), len(encoded))
}

func TestPendingStateResponseEmptyBranchRefsAreAnArray(t *testing.T) {
	for _, refs := range [][]domain.Ref{nil, {}, {{Kind: domain.RefSession, Name: "internal"}, {Kind: domain.RefTag, Name: "bookmark"}, {Kind: domain.RefHead, Name: "HEAD"}}} {
		encoded := pendingStateResponseJSON(t, newPendingStateResponse(domain.PendingStateView{Projection: "on-hold-v1", Refs: refs}))
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["refs"]) != "[]" {
			t.Fatalf("empty response refs must remain an array: %s", fields["refs"])
		}
	}
}

func TestPendingStateProjectedHTTPResponsePreservesDomainAndStoredRefs(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprintf("published=%t", published), func(t *testing.T) {
			repo, view := pendingStateResponseFixture(t)
			parts, err := domain.EncodePendingProjection(context.Background(), repo, view)
			if err != nil {
				t.Fatal(err)
			}
			if published {
				decoded, err := domain.DecodePendingProjection(context.Background(), repo, parts)
				if err != nil || !reflect.DeepEqual(decoded, view) {
					t.Fatal("existing published projection lost internal refs", err)
				}
				view = decoded
				view.ReadGeneration = &domain.PendingReadGeneration{Generation: domain.HashContent([]byte("generation")), SourceEpoch: "12345678-abcd-4321-9876-123456789abc:42", AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:42", Rules: 1, Complete: true}
			}
			before := pendingStateResponseJSON(t, view)
			queries := &repositoryStateHTTPQueries{pending: view}
			server := NewServer(nil, nil)
			if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/pending-view?projection=on-hold-v1", nil)
			request.SetPathValue("repoID", string(repo))
			response := httptest.NewRecorder()
			server.pendingView(response, request)
			var got domain.PendingStateView
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != http.StatusOK {
				t.Fatal("projected HTTP response failed", response.Code, response.Body, err)
			}
			want := view
			want.Refs = append([]domain.Ref{}, view.Refs[:2]...)
			if !reflect.DeepEqual(got, want) || queries.pendingCalls.Load() != 1 {
				t.Fatal("HTTP response changed fields other than refs or reread the projection")
			}
			if !bytes.Equal(before, pendingStateResponseJSON(t, queries.pending)) {
				t.Fatal("HTTP response changed domain serialization")
			}
			persisted := queries.pending
			persisted.ReadGeneration = nil
			reencoded, err := domain.EncodePendingProjection(context.Background(), repo, persisted)
			if err != nil || !reflect.DeepEqual(reencoded, parts) || domain.PendingProjectionRules != 1 {
				t.Fatal("HTTP response changed published rows, order or rules", err)
			}
		})
	}
}

func TestPendingStateResponseDoesNotBypassStoredRefValidation(t *testing.T) {
	repo, view := pendingStateResponseFixture(t)
	parts, err := domain.EncodePendingProjection(context.Background(), repo, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing", "duplicate", "invalid target"} {
		t.Run(mutation, func(t *testing.T) {
			corrupt := append([]domain.PendingProjectionPart{}, parts...)
			changed := false
			for index, part := range corrupt {
				if part.Kind != "ref" {
					continue
				}
				var row struct {
					Version int                `json:"version"`
					RepoID  domain.ContentHash `json:"repo_id"`
					Value   domain.Ref         `json:"value"`
				}
				if err := json.Unmarshal(part.Value, &row); err != nil {
					t.Fatal(err)
				}
				if row.Value.Kind != domain.RefSession {
					continue
				}
				switch mutation {
				case "missing":
					corrupt = append(corrupt[:index], corrupt[index+1:]...)
				case "duplicate":
					corrupt = append(corrupt, part)
				default:
					row.Value.Target = "invalid"
					corrupt[index].Value = pendingStateResponseJSON(t, row)
				}
				changed = true
				break
			}
			if !changed {
				t.Fatal("fixture has no internal session ref")
			}
			if got, err := domain.DecodePendingProjection(context.Background(), repo, corrupt); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.PendingStateView{}) {
				t.Fatal("response-hidden refs escaped durable integrity validation", err)
			}
		})
	}
}
