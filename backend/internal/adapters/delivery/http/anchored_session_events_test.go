package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// anchoredEventsHTTPBackend answers what a caller may read and serves events
// by their place in the document, recording what it was asked for.
type anchoredEventsHTTPBackend struct {
	anchoredNoteHTTPBackend
	access    domain.AnchoredContentAccess
	accessErr error
	reads     []string
	readErr   error
	extra     int
}

func (backend *anchoredEventsHTTPBackend) AnchoredSessionContentAccess(context.Context, domain.ContentHash, domain.ProviderKind, string) (domain.AnchoredContentAccess, error) {
	return backend.access, backend.accessErr
}

func (backend *anchoredEventsHTTPBackend) ReadDocViewEvents(_ context.Context, _, hash domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	backend.reads = append(backend.reads, strings.Join([]string{"events", string(hash), view, itoa(offset), itoa(limit)}, " "))
	page := domain.DocViewPage{Hash: hash, Offset: offset, Total: backend.access.Events}
	// A reader that returns more than it was asked for must not widen what is answered.
	for index := offset; index < offset+limit+backend.extra && index < backend.access.Events; index++ {
		page.Events = append(page.Events, domain.DocViewEvent{Index: index, Event: domain.CIREvent{Kind: domain.EventMessage, Seq: index}})
	}
	return page, backend.readErr
}

func (backend *anchoredEventsHTTPBackend) ReadDocViewFragment(_ context.Context, _, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	backend.reads = append(backend.reads, strings.Join([]string{"fragment", string(hash), itoa(index), itoa(offset), itoa(budget)}, " "))
	return domain.DocViewFragment{Index: index, Offset: offset, Next: -1, Total: 4, JSON: `"ok"`}, backend.readErr
}

func itoa(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func anchoredEventsHTTPFixture(t *testing.T, role domain.MemberRole) (*anchoredEventsHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &anchoredEventsHTTPBackend{}
	backend.repo = domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("c", 32)}
	// Events 0-9 and 30-34 of a 40-event conversation are open to the caller.
	backend.access = domain.AnchoredContentAccess{Doc: domain.HashContent([]byte("content")), Events: 40, Open: [][2]int{{0, 10}, {30, 35}}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	server := NewServer(backend, identity)
	server.documentViews = backend
	return backend, server.Handler(), "/api/v1/repos/" + string(backend.repo.ID) + "/anchored-sessions/claude/session-1"
}

func TestAnchoredSessionEventsStayInsideWhatTheCallerMayRead(t *testing.T) {
	doc := string(domain.HashContent([]byte("content")))
	for _, test := range []struct {
		name, query string
		status      int
		read        string
		first, last int
		next        int
	}{
		{"a page inside an open span", "?offset=0&limit=4", 200, "events " + doc + " all 0 4", 0, 3, 4},
		{"the default page is cut at the end of the span", "?offset=3", 200, "events " + doc + " all 3 7", 3, 9, -1},
		{"the last event of a span", "?offset=9&limit=20", 200, "events " + doc + " all 9 1", 9, 9, -1},
		{"the second open span", "?offset=30", 200, "events " + doc + " all 30 5", 30, 34, -1},
		{"one event in full", "?offset=31&view=raw", 200, "events " + doc + " raw 31 1", 31, 31, 32},
		{"an event that is not open", "?offset=10", 404, "", 0, 0, 0},
		{"between the spans", "?offset=20&limit=1", 404, "", 0, 0, 0},
		{"beyond the conversation", "?offset=40", 404, "", 0, 0, 0},
		{"no offset", "", 422, "", 0, 0, 0},
		{"a negative offset", "?offset=-1", 422, "", 0, 0, 0},
		{"a page larger than allowed", "?offset=0&limit=21", 422, "", 0, 0, 0},
		{"an empty page", "?offset=0&limit=0", 422, "", 0, 0, 0},
		{"another view", "?offset=0&view=chat", 422, "", 0, 0, 0},
		{"the visible projection", "?offset=0&view=all&view=raw", 422, "", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredEventsHTTPFixture(t, domain.RoleMember)
			// The reader offers two events more than asked; they must not be answered.
			backend.extra = 2
			response := bpHTTP(t, handler, http.MethodGet, base+"/events"+test.query, "", true)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 {
				if len(backend.reads) != 0 {
					t.Fatalf("a refused request read the document: %v", backend.reads)
				}
				return
			}
			if len(backend.reads) != 1 || backend.reads[0] != test.read {
				t.Fatalf("read = %v, want %q", backend.reads, test.read)
			}
			var page struct {
				Events []struct {
					Index int `json:"event_index"`
				} `json:"events"`
				Offset int `json:"offset"`
				Next   int `json:"next"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Events) != test.last-test.first+1 ||
				page.Events[0].Index != test.first || page.Events[len(page.Events)-1].Index != test.last || page.Next != test.next || page.Offset != test.first {
				t.Fatalf("answer: %s %v", response.Body.String(), err)
			}
			// The document's hash is the server's alone.
			if strings.Contains(response.Body.String(), doc) || strings.Contains(response.Body.String(), "sha256:") {
				t.Fatalf("the answer names the document: %s", response.Body.String())
			}
		})
	}
}

func TestAnchoredSessionEventFragmentsFollowTheSameRule(t *testing.T) {
	doc := string(domain.HashContent([]byte("content")))
	for _, test := range []struct {
		name, query string
		status      int
		read        string
	}{
		{"an open event", "?index=32&offset=8&budget=1024", 200, "fragment " + doc + " 32 8 1024"},
		{"the defaults", "?index=0", 200, "fragment " + doc + " 0 0 65536"},
		{"an event that is not open", "?index=10", 404, ""},
		{"no event named", "", 404, ""},
		{"a malformed number", "?index=one", 422, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredEventsHTTPFixture(t, domain.RoleMember)
			response := bpHTTP(t, handler, http.MethodGet, base+"/event-fragments"+test.query, "", true)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if (test.read == "") != (len(backend.reads) == 0) || (test.read != "" && backend.reads[0] != test.read) {
				t.Fatalf("read = %v, want %q", backend.reads, test.read)
			}
		})
	}
}

func TestAnchoredSessionEventsNeedAViewerAndASessionTheyMayRead(t *testing.T) {
	for _, path := range []string{"/events?offset=0", "/event-fragments?index=0"} {
		for _, test := range []struct {
			name   string
			role   domain.MemberRole
			authed bool
			access error
			read   error
			status int
		}{
			{"anonymous", domain.RoleOwner, false, nil, nil, 401},
			{"viewer", domain.RoleViewer, true, nil, nil, 200},
			{"member", domain.RoleMember, true, nil, nil, 200},
			{"a session nothing is open of", domain.RoleMember, true, domain.ErrNotFound, nil, 404},
			{"a malformed identifier", domain.RoleMember, true, domain.ErrValidation, nil, 422},
			{"a document that is not ready", domain.RoleMember, true, nil, domain.ErrDocRangeIndexUnavailable, 503},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				backend, handler, base := anchoredEventsHTTPFixture(t, test.role)
				backend.accessErr, backend.readErr = test.access, test.read
				response := bpHTTP(t, handler, http.MethodGet, base+path, "", test.authed)
				if response.Code != test.status {
					t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
				}
				if (test.status == 200 || test.read != nil) != (len(backend.reads) == 1) {
					t.Fatalf("document reads: %v", backend.reads)
				}
			})
		}
	}
}
