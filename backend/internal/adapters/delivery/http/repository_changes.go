package http

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"net/http"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// One inexpensive durable-cursor read per observed repository per server,
// shared by its browsers. No graph scan, subscriber-sized DB polling fan-out,
// or database transaction is held for the lifetime of an HTTP stream.
type repositoryChangeHub struct {
	mu    sync.Mutex
	repos map[domain.ContentHash]*repositoryWatch
}
type repositoryWatch struct {
	cancel context.CancelFunc
	subs   map[chan revisionNotice]bool
	last   *revisionNotice
}
type revisionNotice struct {
	revision domain.RepositoryRevision
	err      error
}

func (h *repositoryChangeHub) subscribe(repo domain.ContentHash, read func(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)) (<-chan revisionNotice, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.repos == nil {
		h.repos = map[domain.ContentHash]*repositoryWatch{}
	}
	watch := h.repos[repo]
	if watch == nil {
		ctx, cancel := context.WithCancel(context.Background())
		watch = &repositoryWatch{cancel: cancel, subs: map[chan revisionNotice]bool{}}
		h.repos[repo] = watch
		go h.run(ctx, repo, watch, read)
	}
	ch := make(chan revisionNotice, 1)
	watch.subs[ch] = true
	if watch.last != nil {
		ch <- *watch.last
	}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(watch.subs, ch)
		if len(watch.subs) == 0 {
			watch.cancel()
			if h.repos[repo] == watch {
				delete(h.repos, repo)
			}
		}
	}
}
func (h *repositoryChangeHub) run(ctx context.Context, repo domain.ContentHash, watch *repositoryWatch, read func(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := read(bounded, repo)
		cancel()
		n := revisionNotice{v, err}
		h.mu.Lock()
		if watch.last == nil || watch.last.err != nil || err != nil || watch.last.revision != v {
			watch.last = &n
			for ch := range watch.subs {
				select {
				case ch <- n:
				default:
					select {
					case <-ch:
					default:
					}
					ch <- n
				}
			}
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) pendingView(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.GetPendingView(r.Context(), s.repoID(r))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	s.respond(w, struct {
		domain.PendingView
		Graph graphwire.State `json:"graph"`
	}{v, graphwire.Encode(*v.Graph)}, nil)
}
func (s *Server) repositoryChanges(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusServiceUnavailable)
		return
	}
	ch, release := s.changes.subscribe(s.repoID(r), s.b.RepositoryRevision)
	defer release()
	// Reconnection re-runs normal membership/token authorization. A revocation
	// cannot leave a permanent, previously authorized stream alive.
	lifetime := time.NewTimer(25 * time.Second)
	defer lifetime.Stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	for {
		select {
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			return
		case n := <-ch:
			if n.err != nil {
				return
			}
			data, err := json.Marshal(n.revision)
			if err != nil {
				return
			}
			// The current durable counters recover any missed notifications, including
			// a restart or a Last-Event-ID older than retained process state.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err = fmt.Fprintf(w, "id: %d:%d:%d\nevent: revision\ndata: %s\n\n", n.revision.Graph, n.revision.Pending, n.revision.Evidence, data); err != nil {
				return
			}
			f.Flush()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
	}
}

func (s *Server) repositoryRevision(w http.ResponseWriter, r *http.Request) {
	v, err := s.b.RepositoryRevision(r.Context(), s.repoID(r))
	s.respond(w, v, err)
}
