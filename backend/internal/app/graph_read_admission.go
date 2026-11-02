package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphReadAdmissionKey struct{}

var errGraphReadNeedsBulk = fmt.Errorf("%w: graph build needs queued admission", domain.ErrResourceOverloaded)

func WithGraphReadResources(ctx context.Context, read func(context.Context) error) error {
	if permit, ok := ctx.Value(resourcePermitKey{}).(*resourcePermit); ok && permit.live.Load() {
		return resourceWork(ctx, ResourceInteractive, read)
	}
	for _, class := range []ResourceClass{ResourceInteractive, ResourceBulk} {
		err := resourceWork(ctx, class, func(bound context.Context) error {
			return read(context.WithValue(bound, graphReadAdmissionKey{}, true))
		})
		if class == ResourceBulk || !errors.Is(err, errGraphReadNeedsBulk) {
			return err
		}
	}
	return errGraphReadNeedsBulk
}

func acquireGraphBuild(ctx context.Context) (context.Context, func(), error) {
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if errors.Is(err, domain.ErrResourceOverloaded) && ctx.Value(graphReadAdmissionKey{}) != nil {
		return bound, release, errGraphReadNeedsBulk
	}
	return bound, release, err
}
