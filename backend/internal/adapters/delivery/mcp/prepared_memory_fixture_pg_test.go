//go:build postgres

package mcp

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
)

func (fixture *effectiveMemorySourceTransportFixture) preparedReader(test *testing.T, service *app.Service) *memoryquery.PreparedQueries {
	test.Helper()
	reader, err := service.PreparedMemoryQueries()
	if err != nil {
		test.Fatal(err)
	}
	return reader
}
