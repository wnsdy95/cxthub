package repositoryquery

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyPinnedScope struct{}

func (*legacyPinnedScope) IsPinnedRead(context.Context) bool {
	return true
}

func TestLegacyGraphGenerationRejectsDeclaredTypedNilReader(test *testing.T) {
	defer func() {
		if failure := recover(); failure != nil {
			test.Fatalf("declared typed-nil source reader panicked: %v", failure)
		}
	}()
	var reader *legacyConstructorProbe
	generation, complete, err := ReadProjectionGraphSourceGeneration(context.Background(), domain.HashContent([]byte(test.Name())), &legacyPinnedScope{}, reader, false)
	if complete || generation != (domain.RepositoryViewSourceCheckpoint{}) || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("declared typed-nil source reader was not rejected", generation, complete, err)
	}
}
