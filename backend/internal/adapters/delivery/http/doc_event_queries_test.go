package http

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func setServiceDocumentEventQueries(test testing.TB, server *Server, service *app.Service) {
	setServiceDocumentQueriesForProfile(test, server, service, inbound.DocumentQueryCompatibility)
}

func setServiceDocumentQueriesForProfile(test testing.TB, server *Server, service *app.Service, profile inbound.DocumentQueryProfile) {
	test.Helper()
	queries, err := service.DocumentQueriesForProfile(profile)
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetDocumentQueries(queries, profile); err != nil {
		test.Fatal(err)
	}
}
