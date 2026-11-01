package http

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRepositoryChangeStateHTTPMetadataContract(t *testing.T) {
	status := domain.PendingProjectionStatus{State: "catching_up", SourceEpoch: "12345678-abcd-4321-9876-123456789abc:42", Rules: 1}
	queries := &repositoryStateHTTPQueries{state: domain.RepositoryChangeState{RepositoryRevision: domain.RepositoryRevision{Graph: 2, Pending: 3}, PendingProjection: &status}}
	server := NewServer(nil, nil)
	if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.repositoryRevision(response, httptest.NewRequest(http.MethodGet, "/revision", nil))
	var got domain.RepositoryChangeState
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != 200 || !got.Equal(queries.state) {
		t.Fatal(response.Code, response.Body, err)
	}
	queries.err = domain.ErrForbidden
	response = httptest.NewRecorder()
	server.repositoryRevision(response, httptest.NewRequest(http.MethodGet, "/revision", nil))
	if response.Code != 403 {
		t.Fatal("authorization error suppressed", response.Code, response.Body)
	}
}

func TestRevisionHubNotifiesSourceAndReadinessWithoutBodyPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sequence atomic.Int64
		var ready atomic.Bool
		hub := repositoryChangeHub{}
		repo := domain.HashContent([]byte(t.Name()))
		read := func(context.Context, domain.ContentHash) (domain.RepositoryChangeState, error) {
			status := domain.PendingProjectionStatus{State: "catching_up", SourceEpoch: "12345678-abcd-4321-9876-123456789abc:1", Rules: 1}
			if sequence.Load() == 2 {
				status.SourceEpoch = "12345678-abcd-4321-9876-123456789abc:2"
			}
			if ready.Load() {
				status.State, status.Generation = "ready", domain.HashContent([]byte("ready generation"))
			}
			return domain.RepositoryChangeState{RepositoryRevision: domain.RepositoryRevision{Graph: 1, Pending: 1}, PendingProjection: &status}, nil
		}
		notices, release := hub.subscribe(repo, read)
		defer release()
		synctest.Wait()
		first := <-notices
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-notices:
			t.Fatal("new pointer for identical status caused notification")
		default:
		}
		sequence.Store(2)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		second := <-notices
		if first.revision.RepositoryRevision != second.revision.RepositoryRevision || first.revision.Equal(second.revision) {
			t.Fatal("unchanged counters hid source mutation")
		}
		ready.Store(true)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		third := <-notices
		if third.revision.PendingProjection.State != "ready" || second.revision.Equal(third.revision) {
			t.Fatal("worker readiness was not announced")
		}
		cached, releaseCached := hub.subscribe(repo, read)
		defer releaseCached()
		if !(<-cached).revision.Equal(third.revision) {
			t.Fatal("reconnect did not receive latest generation")
		}
	})
}
