package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Seed only synthetic stored fixtures. Root publication remains disabled.
func seedRootReadDoc(t *testing.T, dir string, st *store.FSStore, repo domain.ContentHash, events ...domain.CIREvent) (domain.SessionDoc, domain.ConversationManifest) {
	t.Helper()
	identity := domain.DocumentIdentityRootV1
	return seedRootReadDocWithIdentity(t, dir, st, repo, &identity, events...)
}

func seedRootReadDocWithIdentity(t *testing.T, dir string, st *store.FSStore, repo domain.ContentHash, identity *domain.DocumentIdentity, events ...domain.CIREvent) (domain.SessionDoc, domain.ConversationManifest) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: historyEnvelope(), Events: append([]domain.CIREvent{}, events...)}
	for i := range cir.Events {
		cir.Events[i].Seq = i
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PutChunks(systemTestContext(), repo, bodies); err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := rootReadObjectPath(dir, repo, "docs", hash)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if identity != nil {
		if err := st.PutSnapshot(systemTestContext(), domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: *identity, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	return domain.SessionDoc{Hash: hash, Identity: domain.DocumentIdentityRootV1, CIR: cir}, manifest
}

func TestRootReadCannotHideBehindLegacyIndex(t *testing.T) {
	for _, mode := range []string{"missing_snapshot", "legacy_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
			st := store.NewFSStore(dir)
			var identity *domain.DocumentIdentity
			if mode == "legacy_snapshot" {
				legacy := domain.DocumentIdentityLegacy
				identity = &legacy
			}
			doc, _ := seedRootReadDocWithIdentity(t, dir, st, repo, identity, historyMessage(domain.RoleUser, "needle"))
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			index, err := domain.BuildDocReadIndex(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: doc.CIR})
			if err != nil {
				t.Fatal(err)
			}
			index.Hash = doc.Hash
			encoded, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "repos", strings.TrimPrefix(string(repo), "sha256:"), "read-index-v2", strings.TrimPrefix(string(doc.Hash), "sha256:"))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			for suffix, data := range map[string][]byte{"": encoded, ".search": encoded, ".filter": bytes.Repeat([]byte{255}, 32768)} {
				if err := os.WriteFile(path+suffix, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			svc := NewService(st, st, nil, nil, nil)
			assertRejected := func(err error) {
				t.Helper()
				if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
					t.Fatalf("legacy projection admitted root: %v", err)
				}
			}
			_, err = svc.ReadDocEvents(ctx, repo, doc.Hash, "", 1, 1)
			assertRejected(err)
			_, err = svc.ReadDocFragments(ctx, repo, doc.Hash, 1, 0, 1, 100)
			assertRejected(err)
			for _, query := range []string{"needle", "absent"} {
				_, err = svc.SearchDocEvents(ctx, repo, doc.Hash, query, -1, 10)
				assertRejected(err)
			}
			_, err = svc.ReadAgentHistoryPage(ctx, repo, doc.Hash, domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, CoveredBy: doc.Hash})
			assertRejected(err)
		})
	}
}

func TestRootAgentHistoryWaitsForIdentityWireContract(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	root, _ := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "prompt"))
	empty, _ := seedRootReadDoc(t, dir, st, repo)
	legacy := putHistoryDoc(t, st, repo, historyEnvelope(), historyMessage(domain.RoleUser, "prompt"))
	spy := &rootReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	for _, pair := range [][2]domain.ContentHash{{root.Hash, ""}, {empty.Hash, ""}, {legacy.Hash, root.Hash}, {root.Hash, root.Hash}} {
		_, err := svc.ReadAgentHistoryPage(ctx, repo, pair[0], domain.AgentHistoryPageRequest{Before: 0, Limit: 1, MaxBytes: 1, CoveredBy: pair[1]})
		if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || spy.reads != 0 {
			t.Fatalf("root page admitted before wire migration: reads=%d err=%v", spy.reads, err)
		}
	}
}

func rootReadObjectPath(dir string, repo domain.ContentHash, kind string, hash domain.ContentHash) string {
	return filepath.Join(dir, "repos", strings.TrimPrefix(string(repo), "sha256:"), "objects", kind, strings.TrimPrefix(string(hash), "sha256:"))
}

type rootReadSpy struct {
	*store.FSStore
	reads int
	after func()
}

func (s *rootReadSpy) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	s.reads++
	doc, err := s.FSStore.ReadVerifiedDoc(ctx, repo, hash)
	if err == nil && s.after != nil {
		s.after()
	}
	return doc, err
}

func (s *rootReadSpy) MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error) {
	// A root without a persisted search index must still be searched.
	return map[domain.ContentHash]bool{}, nil
}

func TestRootReadEventsFragmentsAndSearchUseVerifiedSource(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	spy := &rootReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	doc, _ := seedRootReadDoc(t, dir, st, repo,
		historyMessage(domain.RoleUser, "root needle \uac80\uc0c9 "+strings.Repeat("x", 600000)),
		historyMessage(domain.RoleAssistant, "answer"))
	base, _ := seedRootReadDoc(t, dir, st, repo, doc.CIR.Events[0])
	page, err := svc.ReadDocEvents(ctx, repo, doc.Hash, "", 0, 1)
	if err != nil || spy.reads != 1 || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], doc.CIR.Events[0]) || page.Next != 1 {
		t.Fatalf("root first page: reads=%d events=%d error=%v", spy.reads, len(page.Events), err)
	}
	page, err = svc.ReadDocEvents(ctx, repo, doc.Hash, base.Hash, -1, 50)
	if err != nil || page.Inherited != 1 || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], doc.CIR.Events[1]) {
		t.Fatalf("root inherited prefix: events=%d error=%v", len(page.Events), err)
	}
	var joined strings.Builder
	index, offset := 0, 0
	for index == 0 {
		fragment, err := svc.ReadDocFragments(ctx, repo, doc.Hash, index, offset, 1, 64<<10)
		if err != nil || len(fragment.Fragments) != 1 {
			t.Fatalf("root fragment error: %v", err)
		}
		joined.WriteString(fragment.Fragments[0].JSON)
		index, offset = fragment.NextIndex, fragment.NextOffset
	}
	var event domain.CIREvent
	if err := json.Unmarshal([]byte(joined.String()), &event); err != nil || !reflect.DeepEqual(event, doc.CIR.Events[0]) {
		t.Fatal("fragment reconstruction differs", err)
	}
	before := spy.reads
	results, err := svc.Search(ctx, inbound.SearchInput{RepoID: repo, Query: "needle"})
	if err != nil || len(results.Hits) != 1 || spy.reads-before != 2 {
		t.Fatalf("root search ignored or repeated sources: hits=%d reads=%d err=%v", len(results.Hits), spy.reads-before, err)
	}
	empty, _ := seedRootReadDoc(t, dir, st, repo)
	page, err = svc.ReadDocEvents(ctx, repo, empty.Hash, "", 0, 1)
	if err != nil || page.Total != 0 || page.Next != -1 || len(page.Events) != 0 {
		t.Fatalf("empty root: %+v %v", page, err)
	}
}

func TestRootReadChecksUnusedTailAndKeepsOwnedRanges(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	doc, manifest := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "first"), historyMessage(domain.RoleAssistant, strings.Repeat("tail", 200000)))
	spy := &rootReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	tail := rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[len(manifest.Chunks)-1].Hash)
	stored, err := os.ReadFile(tail)
	if err != nil {
		t.Fatal(err)
	}
	tamper := func() {
		bad := append([]byte(nil), stored...)
		bad[len(bad)-1] ^= 1
		if err := os.WriteFile(tail, bad, 0600); err != nil {
			t.Fatal(err)
		}
	}
	spy.after = tamper
	page, err := svc.ReadDocEvents(ctx, repo, doc.Hash, "", 1, 1)
	if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0], doc.CIR.Events[1]) {
		t.Fatal("range reread bytes changed after proof", err)
	}
	spy.after = nil
	if _, err := svc.ReadDocEvents(ctx, repo, doc.Hash, "", 0, 1); err == nil {
		t.Fatal("unrequested corrupt tail accepted")
	}
	if _, err := svc.SearchDocEvents(ctx, repo, doc.Hash, "absent", -1, 10); err == nil {
		t.Fatal("empty search result bypassed corrupt tail")
	}
	if err := os.WriteFile(tail, stored, 0600); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.ReadDocEvents(cancelled, repo, doc.Hash, "", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err := svc.ReadDocEvents(ctx, h('2'), doc.Hash, "", 0, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign root readable", err)
	}
}
