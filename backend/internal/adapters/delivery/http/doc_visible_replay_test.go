package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocVisibleHTTPReplayAcceptsTerminalSelectors(test *testing.T) {
	repo, hash := domain.HashContent([]byte("replay repository")), domain.HashContent([]byte("replay document"))
	for _, total := range []int{0, 20} {
		for _, inherited := range []bool{false, true} {
			selector := docVisibleTestSelector(repo, hash, "units")
			selector.Total, selector.Start, selector.Next, selector.Unit = total, total, total, 0
			if inherited {
				selector.Base, selector.Inherited = hash, total
			}
			cursor := encodeDocVisibleTestSelector(test, selector)
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
			backend.read = func(context.Context) (domain.DocVisiblePage, error) {
				return domain.DocVisiblePage{Projection: domain.DocVisibleProjection, Kind: "units", Hash: hash,
					Base: selector.Base, Generation: selector.Generation, Rules: 1, View: selector.View,
					Total: total, Inherited: selector.Inherited, Start: total, From: total, To: total,
					Units: []domain.DocVisiblePageUnit{}, Events: []domain.DocVisibleEvent{}, ReplayCursor: cursor}, nil
			}
			writer := httptest.NewRecorder()
			(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&cursor="+cursor))
			if writer.Code != http.StatusOK || backend.calls != 1 || backend.request != (domain.DocVisiblePageRequest{Cursor: cursor}) ||
				backend.viewCalls != 0 || backend.eventCalls != 0 {
				test.Fatal("terminal replay did not use the exact visible request", writer.Code, writer.Body.String(), backend.request)
			}
			var page domain.DocVisiblePage
			if err := json.Unmarshal(writer.Body.Bytes(), &page); err != nil || page.ReplayCursor != cursor || page.Total != total ||
				page.Inherited != selector.Inherited || page.NextCursor != nil || page.Units == nil || page.Events == nil {
				test.Fatal("terminal replay wire fields changed", writer.Body.String(), err)
			}
			for _, query := range []string{"&view=all", "&offset=0", "&limit=20", "&base=" + string(hash), "&expand=" + cursor} {
				writer = httptest.NewRecorder()
				(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&cursor="+cursor+query))
				if writer.Code != http.StatusUnprocessableEntity || backend.calls != 1 {
					test.Fatal("terminal replay accepted overridden selection", query, writer.Code, backend.calls)
				}
			}
		}
	}
}
