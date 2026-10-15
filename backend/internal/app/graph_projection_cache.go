package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const graphProjectionCacheBytes = 8 << 20
const graphProjectionCacheEntries = 4

type graphProjectionKey struct {
	repo     domain.ContentHash
	revision domain.RepositoryRevision
	inputs   [sha256.Size]byte
	position string
	origin   string
}

type graphProjectionEntry struct {
	key   graphProjectionKey
	value []byte
}

type graphProjectionCache struct {
	mu       sync.Mutex
	entries  []graphProjectionEntry
	bytes    int
	building map[graphProjectionKey]bool
}

func (cache *graphProjectionCache) lookup(key graphProjectionKey) ([]byte, func(), error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return entry.value, nil, nil
		}
	}
	if cache.building[key] || len(cache.building) >= 2 {
		return nil, nil, domain.ErrResourceOverloaded
	}
	if cache.building == nil {
		cache.building = make(map[graphProjectionKey]bool)
	}
	cache.building[key] = true
	return nil, func() {
		cache.mu.Lock()
		delete(cache.building, key)
		cache.mu.Unlock()
	}, nil
}

func (cache *graphProjectionCache) put(key graphProjectionKey, encoded []byte) {
	if len(encoded) > graphProjectionCacheBytes {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := 0; index < len(cache.entries); {
		entry := cache.entries[index]
		if entry.key.repo == key.repo && (entry.key.revision != key.revision || entry.key.position == key.position) {
			cache.bytes -= len(entry.value)
			copy(cache.entries[index:], cache.entries[index+1:])
			cache.entries[len(cache.entries)-1] = graphProjectionEntry{}
			cache.entries = cache.entries[:len(cache.entries)-1]
		} else {
			index++
		}
	}
	for len(cache.entries) > 0 && (len(cache.entries) >= graphProjectionCacheEntries || cache.bytes+len(encoded) > graphProjectionCacheBytes) {
		cache.bytes -= len(cache.entries[0].value)
		cache.entries[0] = graphProjectionEntry{}
		cache.entries = cache.entries[1:]
	}
	cache.entries = append(cache.entries, graphProjectionEntry{key, encoded})
	cache.bytes += len(encoded)
}

func (s *Service) cachedGraphState(ctx context.Context, repo domain.ContentHash, view domain.RepositoryView, position string) (domain.GraphState, error) {
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	state, supported := s.meta.(outbound.RepositoryTransactionState)
	_, writing := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	if !supported || !state.InReadOnlyTransaction(ctx) || writing || view.Revision == (domain.RepositoryRevision{}) {
		return s.admittedGraphState(ctx, view, position)
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.GraphState{}, err
	}
	fingerprint := sha256.New()
	if err := json.NewEncoder(fingerprint).Encode(view); err != nil {
		return domain.GraphState{}, err
	}
	key := graphProjectionKey{repo: repo, revision: view.Revision, position: position, origin: metadata.GitRemoteURL}
	copy(key.inputs[:], fingerprint.Sum(nil))
	encoded, finish, err := s.graphCache.lookup(key)
	if err != nil {
		return domain.GraphState{}, err
	}
	if finish == nil {
		return decodeGraphProjection(ctx, encoded)
	}
	defer finish()
	graph, err := s.admittedGraphState(ctx, view, position)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return domain.GraphState{}, err
	}
	encoded, err = encodeGraphProjection(ctx, graph)
	if errors.Is(err, errGraphProjectionCacheLimit) {
		return graph, ctx.Err()
	}
	if err != nil {
		return domain.GraphState{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	s.graphCache.put(key, encoded)
	return graph, nil
}

func (s *Service) admittedGraphState(ctx context.Context, view domain.RepositoryView, position string) (domain.GraphState, error) {
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return domain.GraphState{}, err
	}
	defer release()
	return s.projectGraphState(bound, view, position)
}
