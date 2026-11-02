package mcp

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentResponseWriter struct {
	*httptest.ResponseRecorder
	write     func()
	cause     error
	deadlines []time.Time
}

func (writer *documentResponseWriter) Write(body []byte) (int, error) {
	writer.write()
	if writer.cause != nil {
		return 0, writer.cause
	}
	return writer.ResponseRecorder.Write(body)
}

func (writer *documentResponseWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}

func (writer *documentResponseWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}

func TestMCPDocumentResponseRetainsAdmissionThroughFinalWrite(test *testing.T) {
	for _, tool := range []string{"context_fetch", "context_search"} {
		for _, outcome := range []string{"complete", "cancel", "write failure", "borrowed"} {
			test.Run(tool+"/"+outcome, func(test *testing.T) {
				fixture := newMemoryAdmissionFixture(test)
				backend := &indexedPageBackend{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{
					repos: []domain.Repo{fixture.repo}, snapshots: map[domain.ContentHash][]domain.Snapshot{}}}, reader: fixture.backend.Service}
				for _, suffix := range []string{"first", "second"} {
					doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage,
						Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("\"needle \\", 1800) + suffix}}}}}}
					canonical, err := domain.CanonicalBytes(doc.CIR)
					if err != nil {
						test.Fatal(err)
					}
					doc.Hash = domain.HashContent(canonical)
					if _, err := fixture.storage.PutDoc(systemTestContext(), fixture.repo.ID, doc); err != nil {
						test.Fatal(err)
					}
					backend.snapshots[fixture.repo.ID] = append(backend.snapshots[fixture.repo.ID], domain.Snapshot{
						ID: doc.Hash, DocHash: doc.Hash, RepoID: fixture.repo.ID, Branch: "main", CreatedAt: time.Unix(200, 0).UTC()})
				}
				fixture.server.context = backend
				setContextQueryTestPort(test, fixture.server, backend)
				queries, err := fixture.backend.Service.DocumentEventQueries()
				if err != nil {
					test.Fatal(err)
				}
				if err := fixture.server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
					test.Fatal(err)
				}
				parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				releaseBorrowed := func() {}
				if outcome == "borrowed" {
					parent, releaseBorrowed, err = app.AcquireResources(parent, app.ResourceBulk)
					if err != nil {
						test.Fatal(err)
					}
					defer releaseBorrowed()
				}
				writes := 0
				assertBulkHeld := func() {
					probe, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer stop()
					_, release, err := app.AcquireResources(probe, app.ResourceBulk)
					if release != nil {
						release()
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						test.Error("final MCP response escaped bulk admission", err)
					}
				}
				writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
					writes++
					assertBulkHeld()
					if outcome == "cancel" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.cause = io.ErrClosedPipe
				}
				args := toolArgs{Repository: string(fixture.repo.ID), Ref: string(backend.snapshots[fixture.repo.ID][0].ID), Query: "needle", Limit: 2}
				fixture.server.Handler().ServeHTTP(writer, documentEventRequest(test, parent, fixture.token, tool, args))
				if writes != 1 || len(writer.deadlines) != 2 || !writer.deadlines[0].Equal(writer.deadlines[1]) {
					test.Fatal("MCP final write or bounded transport deadline missing", writes, writer.deadlines)
				}
				if deadline, _ := parent.Deadline(); !writer.deadlines[0].Equal(deadline) {
					test.Fatal("MCP response extended the earlier parent deadline", deadline, writer.deadlines)
				}
				if outcome != "cancel" && parent.Err() != nil {
					test.Fatal("MCP response cancelled the caller context", parent.Err())
				}
				if outcome == "complete" || outcome == "borrowed" {
					if writer.Code != 200 || strings.Contains(writer.Body.String(), `"isError":true`) || !strings.Contains(writer.Body.String(), "needle") {
						test.Fatal("real document query failed", writer.Code, writer.Body)
					}
				}
				if outcome == "borrowed" {
					assertBulkHeld()
					releaseBorrowed()
				}
				probe, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("MCP response leaked bulk admission", err)
				}
				release()
			})
		}
	}
}
