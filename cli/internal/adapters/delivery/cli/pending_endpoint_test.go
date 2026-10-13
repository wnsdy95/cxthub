package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func authorizeTestEndpoint(t *testing.T, root, origin, endpoint string) {
	t.Helper()
	observed, err := remotecfg.Observe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	binding := remotecfg.EndpointBinding{Version: 1, RemoteURL: origin, RepoID: remotecfg.RepoIDFor(origin), RepositoryID: "repository-fixture", Endpoint: endpoint}
	if err := remotecfg.SetEndpointWithPending(context.Background(), observed, "origin", binding, false, true); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointMergeMigrationRetainsBranchLockScope(t *testing.T) {
	before := mergeEvidence{Version: 1, RepoID: "synthetic-repo", BranchID: "synthetic-branch", Remote: domain.HashContent([]byte("old endpoint"))}
	after := before
	after.Remote = domain.HashContent([]byte("new endpoint"))
	if mergeScopeKey(before) != mergeScopeKey(after) || sameMergeScope(before, after) || mergeDeliveryKey(before) == mergeDeliveryKey(after) {
		t.Fatal("transport move split branch serialization or weakened evidence")
	}
}

func TestEndpointLegacyCaptureExplicitReceipt(t *testing.T) {
	for _, mode := range []string{"authorized", "missing-receipt", "later-endpoint", "race-during-append", "server-identity-mismatch", "source-exhausted", "migration-budget"} {
		t.Run(mode, func(t *testing.T) {
			const origin = "https://capture.example.test/acme/project"
			const endpoint = "https://api.example.test/api/v1"
			fixture := newFrozenDriver(t, origin)
			writeCapturePushOrigin(t, fixture.cwd, origin)
			runLifecycleGit(t, fixture.cwd, "config", "remote.origin.pushurl", capturePushTestGit)
			fixture.c.WakeCommitCapture = func(string) {}
			fixture.c.WakeHistoricalSync = func(string) {}
			ctx := context.Background()
			pass := fixture.attempt(t, "legacy-receipt", domain.DocumentIdentityLegacy, true)
			if err := processFrozenCapture(ctx, fixture.c, fixture.cwd, fixture.cwd, pass); err != nil {
				t.Fatal(err)
			}
			updates, scope := capturePushSelection(pass)
			if err := queueCapturePush(ctx, fixture.c, fixture.cwd, updates, scope); err != nil {
				t.Fatal(err)
			}
			key, before, raw := readCapturePushRequest(t, fixture.cwd)
			if mode == "source-exhausted" || mode == "migration-budget" {
				before.Tries, before.Next = 8, time.Now().Add(time.Hour)
				raw, _ = json.Marshal(before)
				if err := providerfs.WriteRepoFileDurable(fixture.cwd, key, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			proof := fixture.readAttempt(t, pass)
			if before.Intent.ContextTransport != "" || fixture.repo.ID != remotecfg.RepoIDFor(origin) {
				t.Fatal("not a legacy URL-only intent")
			}
			if mode == "missing-receipt" {
				setTestEndpoint(t, fixture.cwd, origin, endpoint)
			} else {
				authorizeTestEndpoint(t, fixture.cwd, origin, endpoint)
			}
			if mode == "later-endpoint" {
				setTestEndpoint(t, fixture.cwd, origin, "https://later.example.test/api/v1")
			}
			spy := capturePushSpy(t, fixture, pass)
			prepared := 0
			fixture.c.PrepareCapturePush = func(_ context.Context, _ string, destination CapturePushDestination, repoID string) (inbound.SyncRepo, error) {
				prepared++
				if repoID != fixture.repo.ID || destination != (CapturePushDestination{URL: origin, Endpoint: endpoint, RepositoryID: "repository-fixture"}) {
					t.Fatal("receipt lost exact destination")
				}
				if mode == "server-identity-mismatch" {
					return nil, remotecfg.ErrEndpointBindingMismatch
				}
				return spy, nil
			}
			spy.before = func() {
				spy.failure = nil
				if mode == "migration-budget" {
					spy.failure = errors.New("synthetic unavailable destination")
					return
				}
				if len(spy.inputs) == 1 {
					spy.failure = domain.ErrSyncConflict
					if mode == "race-during-append" {
						setTestEndpoint(t, fixture.cwd, origin, "https://later.example.test/api/v1")
					}
				}
			}
			if _, err := drainCapturePush(ctx, fixture.c, fixture.cwd, fixture.cwd, fixture.repo.ID); err != nil {
				t.Fatal(err)
			}
			if mode == "migration-budget" {
				for attempt := 0; attempt < 9; attempt++ {
					_, pending, _ := readCapturePushRequest(t, fixture.cwd)
					for route, retry := range pending.MigrationRetries {
						retry.Next = time.Time{}
						pending.setRetry(route, retry)
					}
					encoded, _ := json.Marshal(pending)
					if err := providerfs.WriteRepoFileDurable(fixture.cwd, key, encoded, 0600); err != nil {
						t.Fatal(err)
					}
					authorizeTestEndpoint(t, fixture.cwd, origin, endpoint)
					if _, err := drainCapturePush(ctx, fixture.c, fixture.cwd, fixture.cwd, fixture.repo.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			afterKey, after, retained := readCapturePushRequest(t, fixture.cwd)
			if afterKey != key || after.Intent != before.Intent || !reflect.DeepEqual(proof, fixture.readAttempt(t, pass)) {
				t.Fatal("migration rewrote frozen identity/proof")
			}
			switch mode {
			case "authorized", "source-exhausted":
				if !after.Done || prepared != 1 || len(spy.inputs) != 2 || !spy.inputs[1].Append {
					t.Fatal("authorized legacy capture did not complete bounded retry")
				}
				if after.Tries != before.Tries || len(after.MigrationRetries) != 1 {
					t.Fatal("original counter reset or migration budget missing")
				}
			case "migration-budget":
				if after.Done || after.Tries != 8 || len(spy.inputs) != 8 || len(after.MigrationRetries) != 1 {
					t.Fatal("migration retry budget reset or exceeded")
				}
				for _, retry := range after.MigrationRetries {
					if retry.Tries != 8 {
						t.Fatal("migration budget not durable")
					}
				}
			case "race-during-append":
				if after.Done || after.Tries != before.Tries || len(spy.inputs) != 1 {
					t.Fatal("route race consumed budget or rerouted append")
				}
				for _, retry := range after.MigrationRetries {
					if retry.Tries != 0 {
						t.Fatal("route mismatch consumed migration budget")
					}
				}
			case "server-identity-mismatch":
				if after.Done || after.Tries != before.Tries || len(spy.inputs) != 0 {
					t.Fatal("server identity mismatch consumed budget")
				}
				for _, retry := range after.MigrationRetries {
					if retry.Tries != 0 {
						t.Fatal("server mismatch consumed migration budget")
					}
				}
			default:
				if prepared != 0 || !bytes.Equal(raw, retained) {
					t.Fatal("unmatched receipt changed queued work")
				}
			}
		})
	}
}

func TestEndpointDeferredMergeExplicitReceipt(t *testing.T) {
	for _, mode := range []string{"authorized", "missing-receipt", "later-endpoint", "race-during-pull", "server-identity-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			const origin = "https://capture.example.test/acme/project"
			const endpoint = "https://api.example.test/api/v1"
			root, container, syncer := newMergeDeliveryFixture(t)
			writeCapturePushOrigin(t, root, origin)
			repo := domain.Repo{ID: remotecfg.RepoIDFor(origin), RemoteURL: origin, LocalPath: root}
			container.ResolveRepo = func(context.Context, string) (domain.Repo, error) { return repo, nil }
			syncer.ref.RepoID = repo.ID
			for index := range syncer.snapshots {
				syncer.snapshots[index].RepoID = repo.ID
			}
			ctx := context.Background()
			if err := persistMergeDelivery(ctx, container, root, readIncomingGitRange(root)); err != nil {
				t.Fatal(err)
			}
			key, before := mergeDeliveryJob(t, root)
			if mode == "missing-receipt" {
				setTestEndpoint(t, root, origin, endpoint)
			} else {
				authorizeTestEndpoint(t, root, origin, endpoint)
			}
			if mode == "later-endpoint" {
				setTestEndpoint(t, root, origin, "https://later.example.test/api/v1")
			}
			prepared := 0
			container.PrepareCapturePush = func(_ context.Context, _ string, destination CapturePushDestination, repoID string) (inbound.SyncRepo, error) {
				prepared++
				if repoID != repo.ID || destination != (CapturePushDestination{URL: origin, Endpoint: endpoint, RepositoryID: "repository-fixture"}) {
					t.Fatal("merge receipt lost exact destination")
				}
				if mode == "server-identity-mismatch" {
					return nil, remotecfg.ErrEndpointBindingMismatch
				}
				return syncer, nil
			}
			if mode == "race-during-pull" {
				syncer.onPull = func() { setTestEndpoint(t, root, origin, "https://later.example.test/api/v1") }
			}
			wake, err := pendingMergeDeliveryWake(ctx, root, root, time.Now())
			wantWake := mode == "authorized" || mode == "race-during-pull" || mode == "server-identity-mismatch"
			if err != nil || wake != wantWake {
				t.Fatal("incorrect migration wake decision", wake, err)
			}
			if _, _, err := replayAutomaticMergeDeliveries(ctx, container, root); err != nil {
				t.Fatal(err)
			}
			afterKey, after := mergeDeliveryJob(t, root)
			if afterKey != key || after.EvidenceHash != before.EvidenceHash || !reflect.DeepEqual(after.Evidence, before.Evidence) {
				t.Fatal("merge evidence rewritten")
			}
			switch mode {
			case "authorized":
				if !after.Completed || prepared != 1 || len(syncer.appends) != 1 {
					t.Fatal("authorized legacy merge not delivered")
				}
			case "race-during-pull", "server-identity-mismatch":
				if after.Completed || after.Attempts != before.Attempts || !after.LastAttempt.Equal(before.LastAttempt) || len(syncer.appends) != 0 {
					t.Fatal("merge race consumed budget or appended")
				}
			default:
				if prepared != 0 || !reflect.DeepEqual(before, after) {
					t.Fatal("unmatched merge receipt changed work")
				}
			}
		})
	}
}
