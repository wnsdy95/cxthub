//go:build postgres

package app

import (
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPGCodeApplicabilityAcrossServerInstances(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := systemTestContext()
	st, e := store.NewPostgresStore(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if _, e = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); e != nil {
		t.Fatal(e)
	}
	peer, e := store.NewPostgresStore(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer peer.Close()
	writer := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	reader := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	repo := hh(t.Name() + time.Now().String())
	origin := "https://github.com/example/replicas"
	st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin})
	oid := strings.Repeat("a", 40)
	entry := domain.GitEntry{OID: strings.Repeat("b", 40), Mode: "100755"}
	in := domain.CodeSelection{CodeCommit: oid, SourceCommit: oid, Paths: []string{"file"}}
	before, e := reader.QueryCodeApplicability(ctx, repo, in)
	if e != nil || before.Paths[0].State != "unknown" {
		t.Fatal(before, e)
	}
	provider := &scanReader{deltas: map[string]domain.GitCommitDelta{oid: {Commit: oid, Parents: []string{}, Changes: []domain.GitPathChange{{Path: "file", After: entry}}, Complete: true}}}
	scans, e := NewGitScans(writer, provider)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = scans.ObservePush(ctx, origin, "refs/heads/main", "", oid, false, "push-1"); e != nil {
		t.Fatal(e)
	}
	runOwn := func(limit int) error {
		for i := 0; i < limit; i++ {
			j, e := st.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
			if errors.Is(e, domain.ErrNotFound) {
				return nil
			}
			if e != nil {
				return e
			}
			if e = scans.run(ctx, j); e != nil {
				return e
			}
		}
		return nil
	}
	if e = runOwn(1); e != nil {
		t.Fatal(e)
	}
	pending, e := reader.QueryCodeApplicability(ctx, repo, in)
	if e != nil || pending.Paths[0].State != "unknown" || pending.Reason != "source_delta_pending" {
		t.Fatal("partial publication asserted applied", pending, e)
	}
	if e = runOwn(10); e != nil {
		t.Fatal(e)
	}
	after, e := reader.QueryCodeApplicability(ctx, repo, in)
	if e != nil || after.Paths[0].State != "applied" || after.Relation != "ancestor" || after.StateHash == before.StateHash {
		t.Fatal(after, e)
	}
	if after.Revision.Graph != before.Revision.Graph || after.Revision.Pending != before.Revision.Pending || after.Revision.Evidence <= before.Revision.Evidence {
		t.Fatal("evidence updated graph cursor", before.Revision, after.Revision)
	}
	provider.offline = true
	again, e := reader.QueryCodeApplicability(ctx, repo, in)
	if e != nil || again.StateHash != after.StateHash {
		t.Fatal("read depended on provider", again, e)
	}
}
