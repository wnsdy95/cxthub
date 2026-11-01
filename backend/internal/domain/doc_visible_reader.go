package domain

import (
	"context"
	"maps"
)

const docVisibleDecodedCacheObjects = 8

type DocVisibleReader struct {
	gate       chan struct{}
	generation DocVisibleGeneration
	load       func(context.Context, ContentHash) ([]byte, error)
	cache      docVisibleDecodedCache
}

func NewDocVisibleReader(generation DocVisibleGeneration, load func(context.Context, ContentHash) ([]byte, error)) (*DocVisibleReader, error) {
	if err := generation.Validate(generation.Hash, generation.RangeGeneration); err != nil {
		return nil, err
	}
	if load == nil {
		return nil, ErrValidation
	}
	generation.Views = maps.Clone(generation.Views)
	return &DocVisibleReader{gate: make(chan struct{}, 1), generation: generation, load: load}, nil
}

func (read *DocVisibleReader) Unit(ctx context.Context, mode string, position int, byEvent bool) (DocVisibleUnit, int, error) {
	if err := ctx.Err(); err != nil {
		return DocVisibleUnit{}, 0, err
	}
	if read.gate == nil {
		return DocVisibleUnit{}, 0, ErrIntegrity
	}
	select {
	case read.gate <- struct{}{}:
		defer func() { <-read.gate }()
	case <-ctx.Done():
		return DocVisibleUnit{}, 0, ctx.Err()
	}
	return readDocVisibleUnit(ctx, read.generation, mode, position, byEvent, read.load, &read.cache)
}

type docVisibleDecodedObject struct {
	hash      ContentHash
	directory *docVisibleDirectory
	page      *docVisiblePage
}

type docVisibleDecodedCache struct {
	objects [docVisibleDecodedCacheObjects]docVisibleDecodedObject
	next    int
}

func (cache *docVisibleDecodedCache) restore(hash ContentHash, value any) bool {
	if cache == nil {
		return false
	}
	for _, object := range cache.objects {
		if object.hash != hash {
			continue
		}
		switch decoded := value.(type) {
		case *docVisibleDirectory:
			if object.directory != nil {
				*decoded = *object.directory
				return true
			}
		case *docVisiblePage:
			if object.page != nil {
				*decoded = *object.page
				return true
			}
		}
	}
	return false
}

func (cache *docVisibleDecodedCache) remember(hash ContentHash, value any) {
	if cache == nil {
		return
	}
	object := docVisibleDecodedObject{hash: hash}
	switch decoded := value.(type) {
	case *docVisibleDirectory:
		if len(decoded.Children) == 0 || len(decoded.Children) > DocRangeDirectoryFanout {
			return
		}
		object.directory = decoded
	case *docVisiblePage:
		if len(decoded.Units) == 0 || len(decoded.Units) > DocVisiblePageUnits {
			return
		}
		object.page = decoded
	default:
		return
	}
	cache.objects[cache.next] = object
	cache.next = (cache.next + 1) % len(cache.objects)
}
