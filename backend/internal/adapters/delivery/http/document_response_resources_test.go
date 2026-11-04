package http

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentResponseWriter struct {
	*httptest.ResponseRecorder
	write func()
	cause error
}

func (writer *documentResponseWriter) Write(body []byte) (int, error) {
	writer.write()
	if writer.cause != nil {
		return 0, writer.cause
	}
	return writer.ResponseRecorder.Write(body)
}

func TestDocumentHTTPResponseRetainsAdmissionThroughWrite(test *testing.T) {
	repo, hash := domain.HashContent([]byte("response repo")), domain.HashContent([]byte("response doc"))
	for _, route := range docAllHTTPRoutes(test, repo, hash) {
		for _, outcome := range []string{"complete", "cancel", "write failure"} {
			test.Run(route.name+"/"+outcome, func(test *testing.T) {
				owner := &docAllHTTPQueries{output: route.output}
				owner.read = func(ctx context.Context, method string) error {
					class := app.ResourceInteractive
					if method == "events" || route.name == "legacy find" {
						class = app.ResourceBulk
					}
					bound, release, err := app.AcquireResources(ctx, class)
					if err != nil {
						return err
					}
					if !inbound.RetainDocumentResponse(bound, release) {
						release()
						if method != "events" {
							test.Fatal("document response owner is missing")
						}
					}
					return nil
				}
				server := NewServer(&docAllHTTPBackend{}, nil)
				if err := server.SetDocumentQueries(owner, inbound.DocumentQueryCompatibility); err != nil {
					test.Fatal(err)
				}
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				writes := 0
				writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
					writes++
					_, release, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
					if err != nil {
						test.Fatal("remaining response slot unavailable", err)
					}
					defer release()
					probe, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer stop()
					_, unexpected, err := app.AcquireResources(probe, app.ResourceInteractive)
					if unexpected != nil {
						unexpected()
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						test.Error("response bytes escaped aggregate admission", err)
					}
					if outcome == "cancel" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.cause = io.ErrClosedPipe
				}
				docAllHTTPHandler(server, route.method)(writer, docAllHTTPRequest(repo, hash, route).WithContext(parent))
				if writes != 1 || len(owner.calls) != 1 {
					test.Fatal("response path did not execute once", writes, owner.calls)
				}
				probe, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("response completion leaked admission", err)
				}
				release()
			})
		}
	}
}

func TestDocumentResponseOwnerRejectsLateRetentionAndClosesOnce(test *testing.T) {
	owner := &documentResponseResources{open: true}
	released := 0
	if !owner.retain(func() { released++ }) {
		test.Fatal("live response owner rejected its final read")
	}
	owner.close()
	owner.close()
	if released != 1 || owner.retain(func() { released++ }) || released != 1 {
		test.Fatal("response owner reused or duplicated a released read", released)
	}
}

func TestDocumentHTTPPreparationKeepsIndependentBulkQueue(test *testing.T) {
	repo, hash := domain.HashContent([]byte("queued response repo")), domain.HashContent([]byte("queued response doc"))
	for _, route := range docAllHTTPRoutes(test, repo, hash) {
		if route.name != "view" && route.name != "legacy find" {
			continue
		}
		test.Run(route.name, func(test *testing.T) {
			_, releaseBulk, err := app.AcquireResources(context.Background(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer releaseBulk()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			prepared, done := make(chan struct{}), make(chan struct{})
			owner := &docAllHTTPQueries{output: route.output, read: func(ctx context.Context, _ string) error {
				_, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
				if err != nil {
					return err
				}
				release()
				close(prepared)
				bound, release, err := app.AcquireResources(ctx, app.ResourceBulk)
				if err != nil {
					return err
				}
				if !inbound.RetainDocumentResponse(bound, release) {
					release()
					return errors.New("queued final response has no resource owner")
				}
				return nil
			}}
			server := NewServer(&docAllHTTPBackend{}, nil)
			if err := server.SetDocumentQueries(owner, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			writer := httptest.NewRecorder()
			go func() {
				defer close(done)
				docAllHTTPHandler(server, route.method)(writer, docAllHTTPRequest(repo, hash, route).WithContext(parent))
			}()
			defer func() {
				releaseBulk()
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					test.Error("queued response did not terminate")
				}
			}()
			select {
			case <-prepared:
			case <-time.After(2 * time.Second):
				test.Fatal("interactive preparation did not complete beside existing bulk work")
			}
			select {
			case <-done:
				test.Fatal("transport changed independent bulk queueing into an inherited upgrade", writer.Code, writer.Body)
			case <-time.After(20 * time.Millisecond):
			}
			releaseBulk()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				test.Fatal("bulk release did not resume the document response")
			}
			if writer.Code != 200 || len(owner.calls) != 1 {
				test.Fatal("queued response failed", writer.Code, writer.Body, owner.calls)
			}
		})
	}
}
