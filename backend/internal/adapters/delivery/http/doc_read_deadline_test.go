package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type deadlineDocBackend struct {
	Backend
	read func(context.Context)
}

func (backend deadlineDocBackend) ReadDocViewEvents(ctx context.Context, _, _, _ domain.ContentHash, _, _ int, _ string) (domain.DocViewPage, error) {
	backend.read(ctx)
	return domain.DocViewPage{Next: -1}, nil
}

func (backend deadlineDocBackend) ReadDocViewFragment(ctx context.Context, _, _ domain.ContentHash, _, _, _ int) (domain.DocViewFragment, error) {
	backend.read(ctx)
	return domain.DocViewFragment{Next: -1}, nil
}

type deadlineDocWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
	failure     error
}

func (writer *deadlineDocWriter) SetReadDeadline(deadline time.Time) error {
	writer.read = deadline
	return writer.failure
}

func (writer *deadlineDocWriter) SetWriteDeadline(deadline time.Time) error {
	writer.write = deadline
	return writer.failure
}

func TestDocViewHTTPDeadlineBoundsWholeOperationWithoutOuterPermit(t *testing.T) {
	for _, fragment := range []bool{false, true} {
		for _, parentLimit := range []time.Duration{0, 30 * time.Second} {
			parent := context.Background()
			if parentLimit != 0 {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, parentLimit)
				defer cancel()
			}
			var observed context.Context
			server := &Server{b: deadlineDocBackend{read: func(ctx context.Context) {
				observed = ctx
				probe, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				for _, class := range []app.ResourceClass{app.ResourceBulk, app.ResourceInteractive} {
					_, release, err := app.AcquireResources(probe, class)
					if err != nil {
						t.Fatal("preparation inherited an outer permit", err)
					}
					defer release()
				}
			}}}
			configureDocumentViewTestServer(server)
			path, handler := "/?view=chat", server.getDocEvents
			if fragment {
				path, handler = "/?index=0", server.getDocEventFragment
			}
			writer := &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			handler(writer, httptest.NewRequest(http.MethodGet, path, nil).WithContext(parent))
			if observed == nil || writer.Code != http.StatusOK {
				t.Fatal("viewer did not run", writer.Code, writer.Body)
			}
			deadline, present := observed.Deadline()
			if !present || !writer.read.Equal(deadline) || !writer.write.Equal(deadline) {
				t.Fatal("transport and service deadlines differ", deadline, writer.read, writer.write)
			}
			if parentLimit == 0 {
				if deadline.Before(start.Add(resourceRequestLimit)) || deadline.After(time.Now().Add(resourceRequestLimit)) {
					t.Fatal("request deadline is not bounded", deadline)
				}
			} else if expected, _ := parent.Deadline(); !deadline.Equal(expected) {
				t.Fatal("earlier parent deadline extended", deadline, expected)
			}
			if !errors.Is(observed.Err(), context.Canceled) || parent.Err() != nil {
				t.Fatal("request cancellation leaked or cancelled parent", observed.Err(), parent.Err())
			}
		}
	}
}

func TestDocViewHTTPDeadlineFailureDoesNotCallBackend(t *testing.T) {
	server := &Server{b: deadlineDocBackend{read: func(context.Context) { t.Fatal("unbounded backend call") }}}
	configureDocumentViewTestServer(server)
	for _, handler := range []http.HandlerFunc{server.getDocEvents, server.getDocEventFragment} {
		writer := &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder(), failure: errors.New("deadline failed")}
		handler(writer, httptest.NewRequest(http.MethodGet, "/?view=chat&index=0", nil))
		if writer.Code != http.StatusInternalServerError {
			t.Fatal("deadline failure accepted", writer.Code, writer.Body)
		}
	}
}
