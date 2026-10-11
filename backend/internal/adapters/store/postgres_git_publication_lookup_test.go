//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func putGitLookupEventPG(t *testing.T, ctx context.Context, storage *PostgresStore, event domain.HistoryEvent) {
	t.Helper()
	if err := domain.ValidateHistoryEvent(event); err != nil {
		t.Fatal("invalid lookup fixture", event, err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, event.RepoID, event.ID, raw)
}

func gitPublicationLookupPGFixture(t *testing.T, storage *PostgresStore) (context.Context, domain.ContentHash, domain.HistoryEvent, domain.HistoryEvent) {
	t.Helper()
	ctx, repo := metadataGenerationPGFixture(t, storage)
	publication := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 9000), RepoID: string(repo), BranchID: "source-identity", Branch: "topic", LocalBranch: "feature", WorktreeID: strings.Repeat("1", 32), Kind: "publish", Source: domain.HashContent([]byte("lookup capture")), Target: domain.HashContent([]byte("lookup capture")), GitAfter: strings.Repeat("a", 40), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	ordinary := publication
	ordinary.ID, ordinary.Kind = fmt.Sprintf("%032x", 1000), "position"
	putGitLookupEventPG(t, ctx, storage, ordinary)
	putGitLookupEventPG(t, ctx, storage, publication)
	return ctx, repo, publication, ordinary
}

func TestPGGitPublicationLookupFullKeyAndKinds(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	want := []domain.HistoryEvent{ordinary}
	for index, kind := range []string{"birth", "attach", "advance"} {
		event := ordinary
		event.ID, event.Kind = fmt.Sprintf("%032x", 1001+index), kind
		putGitLookupEventPG(t, ctx, storage, event)
		want = append(want, event)
	}
	for index, change := range []func(*domain.HistoryEvent){
		func(event *domain.HistoryEvent) { event.BranchID = "foreign-identity" },
		func(event *domain.HistoryEvent) { event.Branch = "different-context-name" },
		func(event *domain.HistoryEvent) { event.LocalBranch = "different-native-name" },
		func(event *domain.HistoryEvent) { event.WorktreeID = strings.Repeat("2", 32) },
		func(event *domain.HistoryEvent) { event.Target = domain.HashContent([]byte("other capture")) },
		func(event *domain.HistoryEvent) { event.GitAfter = strings.Repeat("b", 40) },
		func(event *domain.HistoryEvent) { event.Kind = "publish" },
		func(event *domain.HistoryEvent) { event.Kind, event.PreviousBranch = "rename", "previous" },
		func(event *domain.HistoryEvent) { event.Kind = "archive" },
		func(event *domain.HistoryEvent) { event.Kind, event.Source, event.Target = "orphan", "", "" },
	} {
		event := ordinary
		event.ID = fmt.Sprintf("%032x", index+1)
		change(&event)
		putGitLookupEventPG(t, ctx, storage, event)
		exact := query
		exact.IDs = []string{event.ID}
		if got, err := storage.ListGitPublicationWitnesses(ctx, repo, exact); !errors.Is(err, domain.ErrConflict) || got != nil {
			t.Fatal("nonmatching explicit witness accepted", index, got, err)
		}
	}
	got, err := peer.ListGitPublicationWitnesses(ctx, repo, query)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("publication key isolation failed", got, want, err)
	}
	query.GitHead = strings.Repeat("b", 40)
	if got, err := peer.ListGitPublicationWitnesses(ctx, repo, query); err != nil || len(got) != 1 || got[0].GitAfter != query.GitHead || got[0].ID != fmt.Sprintf("%032x", 6) {
		t.Fatal("carry lookup changed more than Git head", got, err)
	}
	query.GitHead = strings.Repeat("c", 40)
	if got, err := peer.ListGitPublicationWitnesses(ctx, repo, query); err != nil || got == nil || len(got) != 0 {
		t.Fatal("missing ordinary evidence was invented", got, err)
	}
	query.GitHead, query.Publication.MemoryPinned = publication.GitAfter, true
	if got, err := peer.ListGitPublicationWitnesses(ctx, repo, query); !errors.Is(err, domain.ErrConflict) || got != nil {
		t.Fatal("caller changed immutable publication pin", got, err)
	}
	query.Publication.ID = fmt.Sprintf("%032x", 9001)
	if _, err := peer.ListGitPublicationWitnesses(ctx, repo, query); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("unaccepted publication supplied authority", err)
	}
	stored, err := peer.GetGitHistoryEvent(ctx, repo, publication.ID)
	if err != nil || !reflect.DeepEqual(stored, publication) {
		t.Fatal("lookup relabeled original publication", stored, err)
	}
}

func TestPGGitPublicationLookupFrozenIDsBeyondLimit(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	for index := 1; index <= 150; index++ {
		event := ordinary
		event.ID = fmt.Sprintf("%032x", 1000+index)
		putGitLookupEventPG(t, ctx, storage, event)
	}
	query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	rows, err := storage.ListGitPublicationWitnesses(ctx, repo, query)
	if err != nil || len(rows) != domain.MaxGitAdmissionWitnesses || rows[0].ID != ordinary.ID || rows[len(rows)-1].ID != fmt.Sprintf("%032x", 1127) {
		t.Fatal("canonical first witnesses changed", len(rows), err)
	}
	query.IDs = []string{fmt.Sprintf("%032x", 1130), fmt.Sprintf("%032x", 1150)}
	before, err := storage.ListGitPublicationWitnesses(ctx, repo, query)
	if err != nil || len(before) != 2 {
		t.Fatal("frozen witnesses outside first page unavailable", before, err)
	}
	late := ordinary
	late.ID = fmt.Sprintf("%032x", 1)
	putGitLookupEventPG(t, ctx, storage, late)
	if got, err := storage.ListGitPublicationWitnesses(ctx, repo, query); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatal("late lower ID displaced frozen witnesses", got, before, err)
	}
	query.IDs = append(query.IDs, fmt.Sprintf("%032x", 1200))
	if got, err := storage.ListGitPublicationWitnesses(ctx, repo, query); !errors.Is(err, domain.ErrConflict) || got != nil {
		t.Fatal("partial frozen witness set returned", got, err)
	}
	for _, ids := range [][]string{{ordinary.ID, ordinary.ID}, {late.ID, "invalid"}, {publication.ID}, {ordinary.ID, late.ID}, make([]string, domain.MaxGitAdmissionWitnesses+1)} {
		query.IDs = ids
		if _, err := storage.ListGitPublicationWitnesses(ctx, repo, query); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid frozen witness query accepted", ids, err)
		}
	}
}

func TestPGGitPublicationLookupBindingsAndForeignOwners(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	if got, err := storage.ListGitBranchBindings(ctx, repo); err != nil || got == nil || len(got) != 0 {
		t.Fatal("nonbinding events returned", got, err)
	}
	if _, err := storage.FindGitForeignHeadOwner(ctx, repo, ordinary.GitAfter, ordinary.BranchID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("publication or own observation became foreign owner", err)
	}
	want := []domain.HistoryEvent{}
	for index := 0; index < 150; index++ {
		event := ordinary
		event.ID, event.BranchID = fmt.Sprintf("%032x", 2000+index), fmt.Sprintf("binding-%d", index)
		event.Kind = []string{"birth", "orphan", "rename", "archive"}[index%4]
		if event.Kind == "orphan" {
			event.Source, event.Target = "", ""
		}
		if event.Kind == "rename" {
			event.PreviousBranch = "previous"
		}
		putGitLookupEventPG(t, ctx, storage, event)
		want = append(want, event)
	}
	if got, err := storage.ListGitBranchBindings(ctx, repo); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("binding list truncated or changed kind/order", len(got), err)
	}
	foreign := ordinary
	foreign.ID, foreign.BranchID, foreign.Branch, foreign.LocalBranch = fmt.Sprintf("%032x", 50), "archived-identity", "old-context", "old-native"
	foreign.Target, foreign.WorktreeID = domain.HashContent([]byte("unrelated capture")), strings.Repeat("3", 32)
	putGitLookupEventPG(t, ctx, storage, foreign)
	archived := foreign
	archived.ID, archived.Kind = fmt.Sprintf("%032x", 51), "archive"
	putGitLookupEventPG(t, ctx, storage, archived)
	empty := foreign
	empty.ID, empty.Target = fmt.Sprintf("%032x", 1), ""
	putGitLookupEventPG(t, ctx, storage, empty)
	if got, err := storage.FindGitForeignHeadOwner(ctx, repo, publication.GitAfter, publication.BranchID); err != nil || !reflect.DeepEqual(got, foreign) {
		t.Fatal("owner query restricted capture/worktree/lifecycle or ignored branch/ID order", got, foreign, err)
	}
	if _, err := storage.FindGitForeignHeadOwner(ctx, repo, strings.Repeat("e", 40), publication.BranchID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("owner crossed Git head", err)
	}
}

func TestPGGitPublicationLookupOptionalScalarsAndMaximumKeys(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	publication.ID, publication.LocalBranch, publication.WorktreeID = fmt.Sprintf("%032x", 9001), "", ""
	putGitLookupEventPG(t, ctx, storage, publication)
	ordinary.ID, ordinary.LocalBranch, ordinary.WorktreeID = fmt.Sprintf("%032x", 1010), "", ""
	putGitLookupEventPG(t, ctx, storage, ordinary)
	explicit := ordinary
	explicit.ID = fmt.Sprintf("%032x", 1011)
	putGitLookupEventPG(t, ctx, storage, explicit)
	catalogExecPG(t, ctx, storage.pool, `UPDATE context_history SET event=event || '{"local_branch":"","worktree_id":""}'::jsonb WHERE repo_id=$1 AND id=$2`, repo, explicit.ID)
	query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	if got, err := storage.ListGitPublicationWitnesses(ctx, repo, query); err != nil || !reflect.DeepEqual(got, []domain.HistoryEvent{ordinary, explicit}) {
		t.Fatal("omitted and explicit empty scalar keys diverged", got, err)
	}
	publication.ID = fmt.Sprintf("%032x", 9002)
	longName := func(seed string) string {
		var name strings.Builder
		for index := 0; name.Len() < 1024; index++ {
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, index)))
			name.WriteString(base64.RawURLEncoding.EncodeToString(hash[:]))
		}
		return "x" + name.String()[1:1024]
	}
	publication.Branch, publication.LocalBranch = longName("context"), longName("native")
	publication.BranchID, publication.WorktreeID, publication.GitAfter = strings.Repeat("c", 128), strings.Repeat("4", 32), strings.Repeat("d", 64)
	putGitLookupEventPG(t, ctx, storage, publication)
	ordinary = publication
	ordinary.ID, ordinary.Kind = fmt.Sprintf("%032x", 1020), "position"
	putGitLookupEventPG(t, ctx, storage, ordinary)
	query = domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	if got, err := storage.ListGitPublicationWitnesses(ctx, repo, query); err != nil || !reflect.DeepEqual(got, []domain.HistoryEvent{ordinary}) {
		t.Fatal("maximum legal key not indexable", got, err)
	}
}

func TestPGGitPublicationLookupScopeSnapshotAndPolicy(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	other := domain.HashContent([]byte(t.Name() + " other"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetGitHistoryEvent(ctx, other, publication.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("history lookup crossed repo", err)
	}
	if _, err := storage.ListGitPublicationWitnesses(ctx, other, query); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("publication lookup crossed repo", err)
	}
	if got, err := storage.ListGitBranchBindings(ctx, other); err != nil || got == nil || len(got) != 0 {
		t.Fatal("binding lookup crossed repo", got, err)
	}
	if got, err := storage.ListGitAdmissionRefs(ctx, other); err != nil || got == nil || len(got) != 0 {
		t.Fatal("ref lookup crossed repo", got, err)
	}
	if _, err := storage.FindGitForeignHeadOwner(ctx, other, ordinary.GitAfter, "other-identity"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("owner lookup crossed repo", err)
	}
	check := func(bound context.Context, reader *PostgresStore, want error) {
		t.Helper()
		_, historyErr := reader.GetGitHistoryEvent(bound, repo, publication.ID)
		_, bindingsErr := reader.ListGitBranchBindings(bound, repo)
		_, witnessesErr := reader.ListGitPublicationWitnesses(bound, repo, query)
		_, ownerErr := reader.FindGitForeignHeadOwner(bound, repo, ordinary.GitAfter, "different-identity")
		_, refsErr := reader.ListGitAdmissionRefs(bound, repo)
		for _, err := range []error{historyErr, bindingsErr, witnessesErr, ownerErr, refsErr} {
			if !errors.Is(err, want) {
				t.Fatal("lookup context/policy contract", err, want)
			}
		}
	}
	before := metadataGenerationPGRead(t, ctx, storage, repo)
	revision, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	check(ctx, storage, nil)
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		check(write, storage, nil)
		check(write, peer, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := metadataGenerationPGRead(t, ctx, peer, repo); got != before {
		t.Fatal("lookups changed catalog", before, got)
	}
	if got, err := peer.RepositoryRevision(ctx, repo); err != nil || got != revision {
		t.Fatal("lookups advanced public revision", revision, got, err)
	}
	if err := storage.WithinRepository(ctx, other, func(write context.Context) error {
		check(write, storage, domain.ErrConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		check(read, peer, domain.ErrConflict)
		check(read, storage, nil)
		later := ordinary
		later.ID = fmt.Sprintf("%032x", 1001)
		putGitLookupEventPG(t, ctx, peer, later)
		if got, err := storage.ListGitPublicationWitnesses(read, repo, query); err != nil || !reflect.DeepEqual(got, []domain.HistoryEvent{ordinary}) {
			t.Fatal("lookup escaped coherent read", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	check(canceled, storage, context.Canceled)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	check(ctx, storage, domain.ErrDocumentIdentityUpgradeRequired)
	capable := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	check(capable, storage, nil)
}

func TestPGGitPublicationLookupCorruptStoredRows(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, mutation := range []string{
		`jsonb_set(event,'{repo_id}',to_jsonb($3::text))`,
		`jsonb_set(event,'{id}',to_jsonb('ffffffffffffffffffffffffffffffff'::text))`,
		`jsonb_set(event,'{created_at}',to_jsonb('invalid'::text))`,
	} {
		t.Run(mutation, func(t *testing.T) {
			ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
			catalogExecPG(t, ctx, storage.pool, `UPDATE context_history SET event=jsonb_set(`+mutation+`,'{kind}','"birth"'::jsonb) WHERE repo_id=$1 AND id=$2 AND $3::text IS NOT NULL`, repo, ordinary.ID, domain.HashContent([]byte("foreign")))
			query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
			_, historyErr := storage.GetGitHistoryEvent(ctx, repo, ordinary.ID)
			_, bindingsErr := storage.ListGitBranchBindings(ctx, repo)
			_, witnessesErr := storage.ListGitPublicationWitnesses(ctx, repo, query)
			_, ownerErr := storage.FindGitForeignHeadOwner(ctx, repo, ordinary.GitAfter, "other-identity")
			query.IDs = []string{ordinary.ID}
			_, exactErr := storage.ListGitPublicationWitnesses(ctx, repo, query)
			for _, err := range []error{historyErr, bindingsErr, witnessesErr, ownerErr, exactErr} {
				if !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt stored evidence accepted", err)
				}
			}
		})
	}
	t.Run("publication", func(t *testing.T) {
		ctx, repo, publication, _ := gitPublicationLookupPGFixture(t, storage)
		catalogExecPG(t, ctx, storage.pool, `UPDATE context_history SET event=jsonb_set(event,'{repo_id}',to_jsonb($3::text)) WHERE repo_id=$1 AND id=$2`, repo, publication.ID, domain.HashContent([]byte("foreign publication")))
		query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
		if got, err := storage.ListGitPublicationWitnesses(ctx, repo, query); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			t.Fatal("corrupt accepted publication supplied witness authority", got, err)
		}
	})
}

func TestPGGitPublicationLookupIndexesAndValidation(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, publication, ordinary := gitPublicationLookupPGFixture(t, storage)
	for index := 0; index < 200; index++ {
		event := ordinary
		event.ID, event.Kind = fmt.Sprintf("%032x", 2000+index), "birth"
		putGitLookupEventPG(t, ctx, storage, event)
	}
	query := domain.GitPublicationWitnessQuery{Publication: publication, GitHead: publication.GitAfter}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := storage.db(read).Exec(read, `SET LOCAL enable_seqscan=off`); err != nil {
			return err
		}
		for _, test := range []struct {
			query, index string
			args         []any
		}{
			{gitBranchBindingsQueryPG, "context_history_git_branch_bindings", []any{repo}},
			{gitPublicationWitnessesQueryPG, "context_history_git_publication_witness", []any{repo, publication.BranchID, publication.Branch, publication.LocalBranch, publication.WorktreeID, publication.Target, query.GitHead, domain.MaxGitAdmissionWitnesses}},
			{gitForeignHeadOwnerBeforeQueryPG, "context_history_git_head_owner", []any{repo, query.GitHead, ordinary.BranchID}},
			{gitForeignHeadOwnerAfterQueryPG, "context_history_git_head_owner", []any{repo, query.GitHead, ordinary.BranchID}},
		} {
			var plan []byte
			if err := storage.db(read).QueryRow(read, `EXPLAIN (FORMAT JSON) `+test.query, test.args...).Scan(&plan); err != nil {
				return err
			}
			if strings.Contains(string(plan), `"Node Type": "Seq Scan"`) || !strings.Contains(string(plan), test.index) {
				t.Fatal("lookup lacks intended indexed path", test.index, string(plan))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "invalid", strings.Repeat("A", 32)} {
		if _, err := storage.GetGitHistoryEvent(ctx, repo, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid history ID accepted", id, err)
		}
	}
	if _, err := storage.ListGitBranchBindings(ctx, "invalid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid repo accepted", err)
	}
	if _, err := storage.ListGitAdmissionRefs(ctx, "invalid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid ref repo accepted", err)
	}
	for _, selection := range [][2]string{{"short", ordinary.BranchID}, {ordinary.GitAfter, ""}, {ordinary.GitAfter, strings.Repeat("a", 129)}} {
		if _, err := storage.FindGitForeignHeadOwner(ctx, repo, selection[0], selection[1]); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid owner query accepted", err)
		}
	}
}

func TestPGGitPublicationLookupAdmissionRefs(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, _, _ := gitPublicationLookupPGFixture(t, storage)
	snapshot := initializationPGSnapshot(t, storage, repo, "lookup refs")
	other := domain.HashContent([]byte("other ref repo"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	otherSnapshot := initializationPGSnapshot(t, storage, other, "foreign lookup ref")
	refs := []domain.Ref{
		{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snapshot.ID, BranchID: "main-id"},
		{RepoID: repo, Kind: domain.RefBranch, Name: "topic", Target: snapshot.ID, BranchID: "topic-id"},
		{RepoID: repo, Kind: domain.RefHead, Name: "HEAD", Symbolic: "refs/heads/main"},
		{RepoID: repo, Kind: domain.RefTag, Name: "unrelated/tag", Target: snapshot.ID},
		{RepoID: other, Kind: domain.RefBranch, Name: "foreign", Target: otherSnapshot.ID, BranchID: "foreign-id"},
	}
	for _, ref := range refs {
		if err := domain.ValidateRef(ref); err != nil {
			t.Fatal(err)
		}
		catalogExecPG(t, ctx, storage.pool, `INSERT INTO refs(repo_id,kind,name,target,symbolic,branch_id) VALUES($1,$2,$3,NULLIF($4,''),$5,$6)`, ref.RepoID, ref.Kind, ref.Name, ref.Target, ref.Symbolic, ref.BranchID)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO refs(repo_id,kind,name,target) SELECT $1,'tag','cxt/history/'||sequence,$2 FROM generate_series(1,512) sequence`, repo, snapshot.ID)
	compare := func() {
		t.Helper()
		all, err := storage.ListRefs(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		want := []domain.Ref{}
		for _, ref := range all {
			if ref.Kind == domain.RefBranch {
				want = append(want, ref)
			}
		}
		sort.Slice(want, func(left, right int) bool { return want[left].Name < want[right].Name })
		if got, err := storage.ListGitAdmissionRefs(ctx, repo); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("admission refs differ from ListRefs branch projection", got, want, err)
		}
	}
	compare()
	archived, err := domain.NewBranchLifecycleRef(repo, "topic", snapshot.ID, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,$2,$3,$4)`, repo, archived.Kind, archived.Name, archived.Target)
	compare()
	if got, err := storage.ListGitAdmissionRefs(ctx, repo); err != nil || len(got) != 1 || got[0] != refs[0] {
		t.Fatal("legacy archived branch was resurrected", got, err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE repos SET context_protocol=1 WHERE id=$1`, repo)
	compare()
	if got, err := storage.ListGitAdmissionRefs(ctx, repo); err != nil || !reflect.DeepEqual(got, refs[:2]) {
		t.Fatal("protocol branch fields changed or legacy tag hid branch", got, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := storage.db(read).Exec(read, `SET LOCAL enable_seqscan=off`); err != nil {
			return err
		}
		var plan []byte
		if err := storage.db(read).QueryRow(read, `EXPLAIN (ANALYZE,FORMAT JSON) `+gitAdmissionRefsQueryPG, repo).Scan(&plan); err != nil {
			return err
		}
		if strings.Contains(string(plan), `"Node Type": "Seq Scan"`) || !strings.Contains(string(plan), "refs_pkey") || !strings.Contains(string(plan), "kind = 'branch'") {
			t.Fatal("branch refs scanned tag history", string(plan))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE refs SET branch_id=$2 WHERE repo_id=$1 AND kind='branch' AND name='main'`, repo, strings.Repeat("x", 129))
	if got, err := storage.ListGitAdmissionRefs(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || got != nil {
		t.Fatal("corrupt branch ref accepted", got, err)
	}
}

type gitLookupPlanNodePG struct {
	NodeType            string                `json:"Node Type"`
	IndexName           string                `json:"Index Name"`
	IndexCondition      string                `json:"Index Cond"`
	RowsRemovedByFilter float64               `json:"Rows Removed by Filter"`
	ActualRows          float64               `json:"Actual Rows"`
	Plans               []gitLookupPlanNodePG `json:"Plans"`
}

func TestPGGitPublicationLookupForeignOwnerBoundedNegative(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, repo, _, ordinary := gitPublicationLookupPGFixture(t, storage)
	raw, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO context_history(repo_id,id,event)
 SELECT $1,lpad(to_hex(sequence),32,'0'),jsonb_set($2::jsonb,'{id}',to_jsonb(lpad(to_hex(sequence),32,'0')))
 FROM generate_series(10000,14095) sequence`, repo, raw)
	if _, err := storage.FindGitForeignHeadOwner(ctx, repo, ordinary.GitAfter, ordinary.BranchID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("same owner became foreign", err)
	}
	catalogExecPG(t, ctx, storage.pool, `ANALYZE context_history`)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		for _, statement := range []string{gitForeignHeadOwnerBeforeQueryPG, gitForeignHeadOwnerAfterQueryPG} {
			var rawPlan []byte
			if err := storage.db(read).QueryRow(read, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+statement, repo, ordinary.GitAfter, ordinary.BranchID).Scan(&rawPlan); err != nil {
				return err
			}
			var plan []struct{ Plan gitLookupPlanNodePG }
			if err := json.Unmarshal(rawPlan, &plan); err != nil || len(plan) != 1 {
				t.Fatal("invalid explain", string(rawPlan), err)
			}
			indexed := false
			var check func(gitLookupPlanNodePG)
			check = func(node gitLookupPlanNodePG) {
				if node.NodeType == "Seq Scan" || node.NodeType == "Sort" || node.RowsRemovedByFilter != 0 || node.ActualRows != 0 {
					t.Fatal("negative owner probe scanned same-owner evidence", string(rawPlan))
				}
				if node.IndexName == "context_history_git_head_owner" && strings.Contains(node.IndexCondition, "branch_id") && (strings.Contains(node.IndexCondition, " < ") || strings.Contains(node.IndexCondition, " > ")) {
					indexed = true
				}
				for _, child := range node.Plans {
					check(child)
				}
			}
			check(plan[0].Plan)
			if !indexed {
				t.Fatal("owner probe lacks bounded index seek", string(rawPlan))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	foreign := ordinary
	foreign.ID, foreign.BranchID = fmt.Sprintf("%032x", 20000), "z-foreign"
	foreign.Target, foreign.WorktreeID = domain.HashContent([]byte("foreign capture")), strings.Repeat("d", 32)
	putGitLookupEventPG(t, ctx, storage, foreign)
	if got, err := storage.FindGitForeignHeadOwner(ctx, repo, ordinary.GitAfter, ordinary.BranchID); err != nil || !reflect.DeepEqual(got, foreign) {
		t.Fatal("foreign owner beyond canonical witnesses was missed", got, err)
	}
	first := foreign
	first.ID, first.BranchID = fmt.Sprintf("%032x", 21000), "a-foreign"
	putGitLookupEventPG(t, ctx, storage, first)
	if got, err := storage.FindGitForeignHeadOwner(ctx, repo, ordinary.GitAfter, ordinary.BranchID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatal("foreign owner was not first branch/ID", got, err)
	}
}
