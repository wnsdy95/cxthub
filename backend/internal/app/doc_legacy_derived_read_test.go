package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocLegacyDerivedChunkRangePreservesOrdinalsAndContext(test *testing.T) {
	ctx := outbound.WithDocReadOnly(context.Background())
	bodies := [][]byte{bytes.Repeat([]byte("a"), domain.ConversationManifestChunkBytes), []byte("tail bytes")}
	descriptors := make([]domain.ConversationManifestChunk, len(bodies))
	for ordinal, body := range bodies {
		descriptors[ordinal] = domain.ConversationManifestChunk{Hash: domain.HashContent(body), Bytes: int64(len(body))}
	}
	var loaded []int
	read := docChunkRangeReader(ctx, int64(len(bodies[0])+len(bodies[1])), func(ordinal int) (domain.ConversationManifestChunk, error) {
		return descriptors[ordinal], nil
	}, func(current context.Context, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
		if current != ctx || !outbound.DocReadOnly(current) || descriptor != descriptors[ordinal] {
			test.Fatal("chunk load lost its context or ordinal", ordinal, descriptor)
		}
		loaded = append(loaded, ordinal)
		return bodies[ordinal], nil
	})
	for _, selection := range []struct {
		offset int
		length int
		want   string
	}{
		{offset: domain.ConversationManifestChunkBytes + 1, length: 3, want: "ail"},
		{offset: domain.ConversationManifestChunkBytes - 4, length: 8, want: "aaaatail"},
		{offset: domain.ConversationManifestChunkBytes + 2, length: 4, want: "il b"},
	} {
		raw, err := read(selection.offset, selection.length)
		if err != nil || string(raw) != selection.want {
			test.Fatal("chunk selection changed bytes", selection, string(raw), err)
		}
	}
	if !reflect.DeepEqual(loaded, []int{1, 0, 1}) {
		test.Fatal("chunk selection changed ordinals or bypassed the cache", loaded)
	}
}

func TestDocLegacyDerivedChunkRangeRejectsInvalidBytesAndPropagatesFailures(test *testing.T) {
	body := []byte("content")
	descriptor := domain.ConversationManifestChunk{Hash: domain.HashContent(body), Bytes: int64(len(body))}
	for _, fixture := range []struct {
		name    string
		body    []byte
		failure error
		want    error
	}{
		{name: "short bytes", body: []byte("short"), want: domain.ErrIntegrity},
		{name: "wrong hash", body: []byte("CHANGED"), want: domain.ErrIntegrity},
		{name: "stale binding", failure: domain.ErrConflict, want: domain.ErrConflict},
		{name: "missing bytes", failure: domain.ErrNotFound, want: domain.ErrNotFound},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			read := docChunkRangeReader(context.Background(), descriptor.Bytes, func(int) (domain.ConversationManifestChunk, error) {
				return descriptor, nil
			}, func(_ context.Context, ordinal int, selected domain.ConversationManifestChunk) ([]byte, error) {
				if ordinal != 0 || selected != descriptor {
					test.Fatal("chunk load received a different selection", ordinal, selected)
				}
				return fixture.body, fixture.failure
			})
			if raw, err := read(0, len(body)); raw != nil || !errors.Is(err, fixture.want) {
				test.Fatal("invalid chunk bytes were exposed or failure was hidden", raw, err)
			}
		})
	}
}
