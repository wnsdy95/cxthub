//go:build postgres

package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"sync"
	"testing"
	"time"
)

func checkStorageAccountingPG(t *testing.T, s *PostgresStore) {
	ctx := context.Background()
	now := time.Now().UTC()
	u := domain.User{ID: "usage-owner", Username: "usage-owner", Name: "Usage", Email: "usage@example.test"}
	if err := s.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	makeRepo := func(ns string, n int) domain.ContentHash {
		w := domain.Repository{ID: domain.NewID("ws_"), Name: fmt.Sprint("Usage", n), Slug: fmt.Sprint("usage", n), OwnerID: u.ID, OwnerUsername: u.Username, OwnerNamespaceID: ns, CreatedAt: now}
		if err := s.CreateRepository(ctx, w); err != nil {
			t.Fatal(err)
		}
		r := domain.Repo{ID: domain.HashContent([]byte(w.ID)), RepositoryID: w.ID, RemoteURL: "https://example.test/" + w.ID, DefaultBranch: "main"}
		if _, err := s.PutRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	ns := domain.NewID("ns_")
	if err := s.CreateNamespace(ctx, domain.Namespace{ID: ns, Slug: "usage-owner", Kind: domain.NamespaceUser, UserID: u.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	a, b := makeRepo(ns, 1), makeRepo(ns, 2)
	bytes := make([]byte, 1024)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(bytes)
	read := func() domain.StorageUsage {
		v, err := s.ReadStorageUsage(ctx, ns, now.Add(-time.Hour), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	upload := func(repo domain.ContentHash, body []byte) error {
		_, _, err := s.PutChunks(ctx, repo, map[domain.ContentHash][]byte{domain.HashContent(body): body})
		return err
	}
	if err := upload(a, bytes); err != nil {
		t.Fatal(err)
	}
	first := read()
	if first.CurrentBytes != int64(len(docCompress(bytes))) || first.State != "metering" {
		t.Fatalf("usage=%+v", first)
	}
	if err := upload(a, bytes); err != nil {
		t.Fatal(err)
	}
	if err := upload(b, bytes); err != nil {
		t.Fatal(err)
	}
	if v := read(); v.CurrentBytes != first.CurrentBytes || len(v.Entries) != len(first.Entries) {
		t.Fatal("duplicate payload charged again")
	}
	// Reconciliation repairs damaged projections, appending corrections without
	// changing archived objects or rewriting ledger entries.
	if _, err := s.pool.Exec(ctx, `UPDATE storage_usage_objects SET bytes=bytes-1 WHERE namespace_id=$1`, ns); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileStorageUsage(ctx, ns); err != nil {
		t.Fatal(err)
	}
	fixed := read()
	if fixed.CurrentBytes != first.CurrentBytes {
		t.Fatalf("reconcile=%d", fixed.CurrentBytes)
	}
	if err := s.ReconcileStorageUsage(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if len(read().Entries) != len(fixed.Entries) {
		t.Fatal("reconciliation is not idempotent")
	}
	policy := domain.StoragePolicy{Plan: "free", IncludedBytes: first.CurrentBytes * 2}
	if err := s.ConfigureStoragePolicy(ctx, ns, "storage-operation", 0, policy, "test-operator", "test provisioning"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureStoragePolicy(ctx, ns, "storage-operation", 0, policy, "test-operator", "test provisioning"); err != nil {
		t.Fatal("replay", err)
	}
	if err := s.ConfigureStoragePolicy(ctx, ns, "storage-operation", 1, policy, "test-operator", "changed command"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("operation identity", err)
	}
	if err := s.ConfigureStoragePolicy(ctx, ns, "new-operation", 0, policy, "test-operator", "stale revision"); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("policy CAS", err)
	}
	var wg sync.WaitGroup
	accepted := make(chan domain.ContentHash, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := append([]byte{}, bytes...)
			body[0] ^= byte(i + 1)
			err := upload(a, body)
			if err == nil {
				accepted <- domain.HashContent(body)
			} else if !errors.Is(err, domain.ErrStorageLimit) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(accepted)
	var added []domain.ContentHash
	for h := range accepted {
		added = append(added, h)
	}
	if len(added) != 1 {
		t.Fatalf("concurrent accepted=%d", len(added))
	}
	full := read()
	if full.CurrentBytes > policy.IncludedBytes || full.State != "read_only" {
		t.Fatalf("cap=%+v", full)
	}
	// Idempotent retry still works at the cap; rejected content did not commit.
	if err := upload(b, bytes); err != nil {
		t.Fatal(err)
	}
	var objects int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1`, a).Scan(&objects); err != nil || objects != 2 {
		t.Fatalf("rolled-back ownership count=%d err=%v", objects, err)
	}
	if _, err := s.GetChunk(ctx, a, hash); err != nil {
		t.Fatal("quota blocked read", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, a, added[0]); err != nil {
		t.Fatal(err)
	}
	if v := read(); v.CurrentBytes != first.CurrentBytes || v.State != "active" {
		t.Fatalf("recovery=%+v", v)
	}
	// A reduction below retained usage restricts growth without deleting data.
	policy.IncludedBytes = 1
	if err := s.ConfigureStoragePolicy(ctx, ns, "reduce-cap", 1, policy, "test-operator", "test read-only"); err != nil {
		t.Fatal(err)
	}
	if read().State != "read_only" {
		t.Fatal("lowered quota not applied")
	}
	future := time.Now().Add(time.Hour)
	policy.GraceBytes = first.CurrentBytes * 4
	policy.GraceUntil = &future
	if err := s.ConfigureStoragePolicy(ctx, ns, "grant-grace", 2, policy, "test-operator", "test recovery window"); err != nil {
		t.Fatal(err)
	}
	if read().State != "grace" {
		t.Fatal("grace not projected")
	}
	if err := upload(a, []byte("grace upload")); err != nil {
		t.Fatal(err)
	}
	// Separate tenant ownership counts the same bytes independently.
	u2 := domain.User{ID: "usage-other", Username: "usage-other", Email: "other@example.test", Name: "Other"}
	if err := s.UpsertUser(ctx, u2); err != nil {
		t.Fatal(err)
	}
	ns2 := domain.NewID("ns_")
	if err := s.CreateNamespace(ctx, domain.Namespace{ID: ns2, Slug: "usage-other", Kind: domain.NamespaceUser, UserID: u2.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	c := makeRepo(ns2, 3)
	if err := upload(c, bytes); err != nil {
		t.Fatal(err)
	}
	other, err := s.ReadStorageUsage(ctx, ns2, now.Add(-time.Hour), time.Now())
	if err != nil || other.CurrentBytes != first.CurrentBytes {
		t.Fatal("tenant dedup scope", err, other.CurrentBytes)
	}
	// Current team defaults are also metered (not only archived settings).
	beforeDefaults, _ := s.ReadStorageUsage(ctx, ns2, now.Add(-time.Hour), time.Now())
	if err := s.PutSettingsBundle(ctx, c, domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: "e30="}}}); err != nil {
		t.Fatal(err)
	}
	afterDefaults, _ := s.ReadStorageUsage(ctx, ns2, now.Add(-time.Hour), time.Now())
	if afterDefaults.CurrentBytes <= beforeDefaults.CurrentBytes {
		t.Fatal("team defaults not counted")
	}
	// Lazy conversion of a previously retained legacy document must still be
	// readable when a later entitlement makes the account read-only.
	legacy := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("retained archive ", 1024)}}}}}}
	raw, _ := domain.CanonicalBytes(legacy.CIR)
	legacy.Hash = domain.HashContent(raw)
	if _, err := s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, legacy.Hash, docCompress(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, c, legacy.Hash); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureStoragePolicy(ctx, ns2, "legacy-cap", 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "test", "quota reduction"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDocManifest(ctx, c, legacy.Hash); err != nil {
		t.Fatal("quota blocked retained legacy read", err)
	}
	if _, err := s.GetDoc(ctx, c, legacy.Hash); err != nil {
		t.Fatal(err)
	}
	// A namespace transfer may not bypass the destination cap. Both the source
	// credit and target charge roll back when the target cannot accept growth.
	rp, err := s.GetRepo(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	moving, err := s.GetRepository(ctx, rp.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	moving.OwnerNamespaceID = ns2
	moving.OwnerID = u2.ID
	moving.OwnerUsername = u2.Username
	if err := s.CreateRepository(ctx, moving); !errors.Is(err, domain.ErrStorageLimit) {
		t.Fatal("transfer bypassed cap", err)
	}
	stayed, err := s.GetRepository(ctx, rp.RepositoryID)
	if err != nil || stayed.OwnerNamespaceID != ns {
		t.Fatal("failed transfer changed ownership", err)
	}
	// Integrate independently recorded historical samples: 1000 excess bytes for
	// half an hour followed by 2000 for half an hour = 1500 byte-hours.
	start := now.Add(-3 * time.Hour)
	end := start.Add(time.Hour)
	for i, n := range []int{2000, 3000} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO storage_usage_ledger(namespace_id,delta_bytes,bytes_after,included_bytes,payg,plan,reason,occurred_at) VALUES($1,0,$2,1000,true,'team','test.sample',$3)`, ns2, n, start.Add(time.Duration(i)*30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	period, err := s.ReadStorageUsage(ctx, ns2, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if period.OverageByteHours != "1500.0000000000000000" && period.OverageByteHours != "1500.00000000000000000000" {
		var got float64
		if _, err := fmt.Sscan(period.OverageByteHours, &got); err != nil || got != 1500 {
			t.Fatal("integration", period.OverageByteHours)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE storage_usage_ledger SET delta_bytes=0 WHERE namespace_id=$1`, ns); err == nil {
		t.Fatal("ledger rewrite allowed")
	}
}
