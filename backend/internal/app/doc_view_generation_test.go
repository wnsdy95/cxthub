package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocViewInheritedReadsLazyPageHashesAndPropagatesFailures(t *testing.T) {
	ctx := context.Background()
	calls := 0
	block := func(page int) (domain.ContentHash, error) {
		calls++
		if page < 0 || page > 1 {
			t.Fatal("unrelated page was read", page)
		}
		return h('1'), nil
	}
	source := docViewSource{total: 200, block: block, location: func(int) (domain.DocEventIndex, error) {
		t.Fatal("equal authenticated pages read individual entries")
		return domain.DocEventIndex{}, nil
	}}
	if inherited, err := docViewInherited(ctx, source, source); err != nil || inherited != 200 || calls != 4 {
		t.Fatal(inherited, calls, err)
	}
	broken := source
	broken.block = func(int) (domain.ContentHash, error) { return "", domain.ErrIntegrity }
	for _, pair := range [][2]docViewSource{{broken, source}, {source, broken}} {
		if _, err := docViewInherited(ctx, pair[0], pair[1]); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("directory failure was hidden", err)
		}
	}
}
