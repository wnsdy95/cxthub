package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMinimumMemoryValidatesRequestBeforeStorage(test *testing.T) {
	service := &Service{}
	repo := domain.HashContent([]byte(test.Name()))
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: repo, CodeCommit: strings.Repeat("a", 40)}}
	if _, err := service.ObserveMemoryReadRequirement(context.Background(), repo, request); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("absent read port", err)
	}
	request.Cursor = "already-started"
	if _, err := service.ObserveMemoryReadRequirement(context.Background(), repo, request); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("continued request observed a new minimum", err)
	}
	request.Cursor = ""
	if page, err := service.QueryMinimumMemory(context.Background(), repo, request, domain.MemoryReadRequirement{}, ""); !errors.Is(err, domain.ErrValidation) || page.Status.Generation != "" || len(page.Page.Items) != 0 {
		test.Fatal("invalid floor accessed storage or returned a page", page, err)
	}
}
