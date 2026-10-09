package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

const capturePushTestContext = "https://example.invalid/team/context"
const capturePushTestGit = "https://example.invalid/code.git"

func newCapturePushDriver(t *testing.T) frozenDriver {
	t.Helper()
	f := newFrozenDriver(t)
	// A push-only remote leaves the synthetic repository's existing identity
	// intact; neither Git nor the Sync spy ever contacts this destination.
	runLifecycleGit(t, f.cwd, "config", "remote.origin.pushurl", capturePushTestGit)
	writeCapturePushOrigin(t, f.cwd, capturePushTestContext)
	got, err := f.c.ResolveRepo(context.Background(), f.cwd)
	if err != nil || got.ID != f.repo.ID {
		t.Fatal("push fixture changed repository identity", err)
	}
	f.c.PrepareCapturePush = func(_ context.Context, cwd string, destination CapturePushDestination, repo string) (inbound.SyncRepo, error) {
		if cwd != f.cwd || destination != (CapturePushDestination{URL: capturePushTestContext}) || repo != f.repo.ID {
			t.Fatal("factory did not receive the exact observed destination and repository")
		}
		return f.c.Sync, nil
	}
	f.c.WakeCommitCapture = func(string) {}
	f.c.WakeHistoricalSync = func(string) {}
	return f
}

func writeCapturePushOrigin(t *testing.T, root, url string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"remotes": map[string]string{"origin": url}})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileDurable(root, ".cxt/config", raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func capturePushSelection(p *commitCapturePass) ([]gitPushUpdate, domain.PublicationScope) {
	local := p.Proof.LocalBranch
	if local == "" {
		local = p.Proof.Branch
	}
	return []gitPushUpdate{{LocalRef: "refs/heads/" + local, LocalOID: p.Proof.GitAfter, RemoteRef: "refs/heads/" + p.Proof.Branch, RemoteOID: strings.Repeat("0", 40)}},
		domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: p.Proof.Branch, BranchID: p.Proof.BranchID}}}
}

func readCapturePushRequest(t *testing.T, root string) (string, capturePushRequest, []byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, capturePushDir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("expected one durable push request", len(paths), err)
	}
	rel, err := filepath.Rel(root, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := providerfs.ReadRepoFile(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	var request capturePushRequest
	if err := json.Unmarshal(raw, &request); err != nil || filepath.Base(rel) != request.Intent.id()+".json" {
		t.Fatal("invalid durable push request", err)
	}
	info, err := os.Stat(paths[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("push request permissions", err)
	}
	return rel, request, raw
}

type capturePushSyncSpy struct {
	inbound.SyncRepo
	t        *testing.T
	store    *storage.FileStore
	branch   domain.PublicationBranch
	target   domain.ContentHash
	inputs   []inbound.SyncInput
	failure  error
	accepted int
	before   func()
}

func (s *capturePushSyncSpy) Push(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.t.Helper()
	if in.Publication == nil || in.Publication.HistoryOnly || in.Force || in.Ref != "" || !in.ForegroundOnly || !reflect.DeepEqual(in.Publication.Branches, []domain.PublicationBranch{s.branch}) || !reflect.DeepEqual(in.Publication.ExpectedTargets, map[string]domain.ContentHash{s.branch.BranchID: s.target}) {
		s.t.Fatal("deferred push broadened or retargeted the frozen scope")
	}
	copy := in
	scope := *in.Publication
	scope.Branches = append([]domain.PublicationBranch(nil), scope.Branches...)
	scope.ExpectedTargets = map[string]domain.ContentHash{s.branch.BranchID: in.Publication.ExpectedTargets[s.branch.BranchID]}
	copy.Publication = &scope
	s.inputs = append(s.inputs, copy)
	if s.before != nil {
		s.before()
	}
	// Model the Sync boundary's ExpectedTargets fence against actual current
	// FileStore refs. An attempt may reach the boundary but cannot publish a
	// newer target. Domain planner tests independently exercise the real fence.
	ref, err := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, s.branch.Branch)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	if ref.BranchID != s.branch.BranchID || ref.Target != s.target {
		return inbound.SyncOutput{}, domain.ErrSyncConflict
	}
	if s.failure != nil {
		return inbound.SyncOutput{}, s.failure
	}
	s.accepted++
	return inbound.SyncOutput{}, nil
}

func capturePushSpy(t *testing.T, f frozenDriver, p *commitCapturePass) *capturePushSyncSpy {
	t.Helper()
	return &capturePushSyncSpy{t: t, store: f.store, branch: domain.PublicationBranch{Branch: p.Proof.Branch, BranchID: p.Proof.BranchID}, target: p.Proof.Target}
}

type capturePushForegroundFailure struct {
	inbound.SyncRepo
	push func(inbound.SyncInput) error
}

func (s capturePushForegroundFailure) Push(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	return inbound.SyncOutput{}, s.push(in)
}

func TestCapturePushGitHookWakesAfterEnqueueAndForegroundFailure(t *testing.T) {
	f := newCapturePushDriver(t)
	p := f.attempt(t, "hook-deferred-push", domain.DocumentIdentityRootV1, true)
	// A completed capture reaches transport. An unfinished one correctly
	// stops earlier at the foreground capture-proof gate.
	if err := processFrozenCapture(context.Background(), f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	update := updates[0]
	setGitPushInput(t, update.LocalRef+" "+update.LocalOID+" "+update.RemoteRef+" "+update.RemoteOID+"\n")
	var order []string
	var queued capturePushIntent
	f.c.Sync = capturePushForegroundFailure{push: func(in inbound.SyncInput) error {
		if in.Publication == nil || !reflect.DeepEqual(in.Publication.Branches, scope.Branches) || !in.ForegroundOnly || in.Force || in.Append {
			t.Fatal("foreground hook lost its selected scope")
		}
		_, request, _ := readCapturePushRequest(t, f.cwd)
		if request.Intent.AttemptID != p.Proof.ID || request.Intent.Update != update || request.Done || request.Tries != 0 || len(order) != 0 {
			t.Fatal("foreground transport preceded durable enqueue")
		}
		queued = request.Intent
		order = append(order, "foreground-failed")
		return errors.New("synthetic foreground transport failure")
	}}
	f.c.WakeCommitCapture = func(cwd string) {
		if cwd != f.cwd || !reflect.DeepEqual(order, []string{"foreground-failed"}) {
			t.Fatal("capture wake preceded foreground failure or ran more than once")
		}
		_, request, _ := readCapturePushRequest(t, cwd)
		if request.Intent != queued || request.Done || request.Tries != 0 {
			t.Fatal("deferred wake cannot discover the original queued request")
		}
		order = append(order, "capture-wake")
	}
	// Enter through the production hook dispatcher, whose outer defer owns
	// capture wake. The callback seam avoids starting a real child process.
	if err := runGitHook(context.Background(), f.c, f.cwd, []string{"pre-push", "origin", capturePushTestGit}); err != nil {
		t.Fatal("pre-push must remain fail-open", err)
	}
	if !reflect.DeepEqual(order, []string{"foreground-failed", "capture-wake"}) {
		t.Fatal("hook lost or duplicated its deferred capture wake", order)
	}
}

func TestCapturePushPendingCompletionReloadAndIdempotence(t *testing.T) {
	f := newCapturePushDriver(t)
	p := f.attempt(t, "queued-push", domain.DocumentIdentityRootV1, true)
	updates, scope := capturePushSelection(p)
	ctx := context.Background()
	if _, err := f.store.CreateBranchRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "unrelated", RepoID: f.repo.ID, Target: p.Initial}); err != nil {
		t.Fatal(err)
	}
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	_, request, before := readCapturePushRequest(t, f.cwd)
	if request.Intent.AttemptID != p.Proof.ID || request.Intent.WorktreeID != p.Proof.WorktreeID || request.Intent.RepoID != f.repo.ID || request.Intent.Update != updates[0] || request.Intent.Branch != scope.Branches[0] || request.Tries != 0 || request.Done {
		t.Fatal("request lost the frozen pre-push selection")
	}
	if bytes.Contains(before, []byte(capturePushTestGit)) || bytes.Contains(before, []byte(capturePushTestContext)) {
		t.Fatal("request persisted raw destinations")
	}
	spy := capturePushSpy(t, f, p)
	f.c.Sync = spy
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() || len(spy.inputs) != 0 {
		t.Fatal("pending capture consumed a transport attempt", err)
	}
	if _, _, raw := readCapturePushRequest(t, f.cwd); !bytes.Equal(before, raw) {
		t.Fatal("pending/idempotent queue changed the receipt")
	}
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	f.published(t, p)
	spy.target = p.Proof.Target
	// New adapters and a copied Container must discover everything from disk.
	fresh := storage.NewWorktreeFileStore(f.cwd, gitOut(f.cwd, "rev-parse", "--absolute-git-dir"), "main", p.Proof.GitAfter)
	reloaded := *f.c
	reloaded.History = app.NewContextHistoryService(fresh, fresh)
	spy.store = fresh
	if next, err := drainCapturePush(ctx, &reloaded, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() {
		t.Fatal("completed push did not drain", err)
	}
	_, done, doneRaw := readCapturePushRequest(t, f.cwd)
	if !done.Done || done.Tries != 1 || done.Error != "" || done.Intent != request.Intent || spy.accepted != 1 || len(spy.inputs) != 1 || spy.inputs[0].Append || spy.inputs[0].RepoID != f.repo.ID || spy.inputs[0].Cwd != f.cwd {
		t.Fatal("exact deferred push was not acknowledged once")
	}
	if err := queueCapturePush(ctx, &reloaded, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCapturePush(ctx, &reloaded, f.cwd, f.cwd, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, raw := readCapturePushRequest(t, f.cwd); !bytes.Equal(doneRaw, raw) || len(spy.inputs) != 1 {
		t.Fatal("completed request was reset or redelivered")
	}
}

type capturePushHistoryView struct {
	inbound.ContextHistory
	replacementID string
	hidePublish   string
	forgePublish  bool
}

func (h capturePushHistoryView) ResolveLocalBranch(ctx context.Context, repo, local string) (domain.LocalBranchBinding, error) {
	b, err := h.ContextHistory.ResolveLocalBranch(ctx, repo, local)
	if err == nil && h.replacementID != "" {
		b.BranchID = h.replacementID
	}
	return b, err
}

func (h capturePushHistoryView) ListHistory(ctx context.Context, repo string) ([]domain.HistoryEvent, error) {
	events, err := h.ContextHistory.ListHistory(ctx, repo)
	if err != nil {
		return nil, err
	}
	var filtered []domain.HistoryEvent
	for _, event := range events {
		if event.ID == h.hidePublish {
			if !h.forgePublish {
				continue
			}
			event.Target = domain.HashContent([]byte("different publication target"))
		}
		filtered = append(filtered, event)
	}
	return filtered, nil
}

func TestCapturePushChangedBindingsAndMissingPublicationStayPending(t *testing.T) {
	for _, reason := range []string{"git-ref", "branch-identity", "git-origin", "context-origin", "no-local-publication", "wrong-publication", "target-mismatch"} {
		t.Run(reason, func(t *testing.T) {
			f := newCapturePushDriver(t)
			p := f.attempt(t, reason, domain.DocumentIdentityLegacy, true)
			ctx := context.Background()
			if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
				t.Fatal(err)
			}
			updates, scope := capturePushSelection(p)
			if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
				t.Fatal(err)
			}
			_, queued, _ := readCapturePushRequest(t, f.cwd)
			spy := capturePushSpy(t, f, p)
			f.c.Sync = spy
			switch reason {
			case "git-ref":
				runLifecycleGit(t, f.cwd, "commit", "--allow-empty", "-qm", "different revision")
			case "branch-identity":
				f.c.History = capturePushHistoryView{ContextHistory: f.c.History, replacementID: domain.LegacyContextBranchID(f.repo.ID, "replacement")}
			case "git-origin":
				runLifecycleGit(t, f.cwd, "config", "remote.origin.pushurl", "https://example.invalid/other.git")
			case "context-origin":
				writeCapturePushOrigin(t, f.cwd, "https://example.invalid/team/other")
			case "no-local-publication", "wrong-publication":
				f.c.History = capturePushHistoryView{ContextHistory: f.c.History, hidePublish: domain.CaptureAttempt(*p).Publication().ID, forgePublish: reason == "wrong-publication"}
			case "target-mismatch":
				ref, err := f.store.GetRef(ctx, f.repo.ID, domain.RefBranch, p.Proof.Branch)
				if err != nil {
					t.Fatal(err)
				}
				ref.Target = p.Initial
				if err := f.store.PutRef(ctx, ref); err != nil {
					t.Fatal(err)
				}
			}
			next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID)
			if err != nil || next.IsZero() {
				t.Fatal("rejected request was not durably deferred", err)
			}
			_, failed, _ := readCapturePushRequest(t, f.cwd)
			if failed.Done || failed.Error == "" || failed.Tries != 1 || failed.Intent != queued.Intent || spy.accepted != 0 {
				t.Fatal("changed binding/target was acknowledged")
			}
			wantCalls := 0
			if reason == "target-mismatch" {
				wantCalls = 2
			}
			if len(spy.inputs) != wantCalls || (wantCalls == 2 && (spy.inputs[0].Append || !spy.inputs[1].Append)) {
				t.Fatal("unexpected widened transport retry", len(spy.inputs))
			}
		})
	}
}

func TestCapturePushQueueIgnoresUnsealedLegacyAndUnmatchedCode(t *testing.T) {
	for _, reason := range []string{"unsealed", "legacy", "different-code", "different-identity"} {
		t.Run(reason, func(t *testing.T) {
			f := newCapturePushDriver(t)
			p := f.attempt(t, reason, domain.DocumentIdentityLegacy, reason != "unsealed")
			if reason == "legacy" {
				p.Version, p.InputsReady = 1, false
				p.FrozenPosition, p.Author, p.Settings = nil, nil, nil
				p.Message, p.Outcomes[0].Input = "", nil
				if err := p.write(f.cwd); err != nil {
					t.Fatal(err)
				}
			}
			updates, scope := capturePushSelection(p)
			if reason == "different-code" {
				updates[0].LocalOID = strings.Repeat("a", 40)
			}
			if reason == "different-identity" {
				scope.Branches[0].BranchID = domain.LegacyContextBranchID(f.repo.ID, "other")
			}
			err := queueCapturePush(context.Background(), f.c, f.cwd, updates, scope)
			if reason == "different-identity" {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatal("mismatched scope accepted", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(f.cwd, capturePushDir, "*.json"))
			if err != nil || len(paths) != 0 {
				t.Fatal("unqualified capture became push authority", err)
			}
		})
	}
}

func TestCapturePushLostAcknowledgementPersistsBoundedRetry(t *testing.T) {
	f := newCapturePushDriver(t)
	p := f.attempt(t, "lost-ack", domain.DocumentIdentityLegacy, true)
	ctx := context.Background()
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	spy := capturePushSpy(t, f, p)
	spy.failure = errors.New("synthetic acknowledgement unavailable")
	spy.before = func() {
		_, claimed, _ := readCapturePushRequest(t, f.cwd)
		if claimed.Tries != 1 || claimed.Done || claimed.Next.IsZero() {
			t.Fatal("transport ran before durable retry claim")
		}
	}
	f.c.Sync = spy
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || next.IsZero() {
		t.Fatal(err)
	}
	rel, failed, raw := readCapturePushRequest(t, f.cwd)
	if failed.Done || failed.Tries != 1 || failed.Error == "" || len(spy.inputs) != 1 {
		t.Fatal("missing acknowledgement erased retry")
	}
	// Pin a synthetic future schedule so a slow Git lookup cannot turn this
	// reload assertion into a race against the product's two-second backoff.
	failed.Next = time.Now().Add(time.Hour)
	raw, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileDurable(f.cwd, rel, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, current := readCapturePushRequest(t, f.cwd); !bytes.Equal(current, raw) || len(spy.inputs) != 1 {
		t.Fatal("reload reset backoff or retried early")
	}
	// Advance only the synthetic scheduler record, without sleeps or a real
	// network retry loop. The eighth claim must remain terminal after requeue.
	failed.Tries, failed.Next = 7, time.Time{}
	raw, err = json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileDurable(f.cwd, rel, raw, 0600); err != nil {
		t.Fatal(err)
	}
	spy.before = nil
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() {
		t.Fatal("exhausted retry remained scheduled", err)
	}
	_, exhausted, before := readCapturePushRequest(t, f.cwd)
	if exhausted.Done || exhausted.Tries != 8 || len(spy.inputs) != 2 {
		t.Fatal("wrong final retry state")
	}
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, after := readCapturePushRequest(t, f.cwd); !bytes.Equal(before, after) || len(spy.inputs) != 2 {
		t.Fatal("requeue reset exhausted transport budget")
	}
}

func TestCapturePushAtomicTempsDoNotBlock(t *testing.T) {
	f := newCapturePushDriver(t)
	ctx := context.Background()
	p := f.attempt(t, "atomic-temp-push", domain.DocumentIdentityLegacy, true)
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.cwd, capturePushDir)
	killed, err := os.CreateTemp(dir, ".cxt-write-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := killed.WriteString("{partial"); err != nil {
		t.Fatal(err)
	}
	if err := killed.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := os.CreateTemp(dir, ".cxt-write-*")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	// A writer remains open throughout the scan, then finishes while transport
	// runs. Channels establish ordering without sleeps or scheduler assumptions.
	release, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		<-release
		_, err := active.WriteString("{still-partial")
		if err == nil {
			err = active.Close()
		}
		if err == nil {
			err = os.Remove(active.Name())
		}
		finished <- err
	}()
	var released bool
	defer func() {
		if !released {
			close(release)
			<-finished
		}
	}()
	spy := capturePushSpy(t, f, p)
	spy.before = func() {
		close(release)
		released = true
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	f.c.Sync = spy
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() {
		t.Fatal(next, err)
	}
	_, done, _ := readCapturePushRequest(t, f.cwd)
	if !done.Done || spy.accepted != 1 {
		t.Fatal("atomic temp poisoned delivery")
	}
	if raw, err := os.ReadFile(killed.Name()); err != nil || string(raw) != "{partial" {
		t.Fatal("drain altered retained writer residue", err)
	}
}

func TestCapturePushAtomicTempValidation(t *testing.T) {
	for _, kind := range []string{"renamed", "symlink", "directory", "fifo", "malformed-name", "other-file", "malformed-request"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, capturePushDir)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			name := ".cxt-write-1234"
			path := filepath.Join(dir, name)
			switch kind {
			case "symlink":
				if err := os.Symlink("missing-private-target", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "malformed-name" {
					path = filepath.Join(dir, ".cxt-write-not-a-writer")
				}
				if kind == "other-file" {
					path = filepath.Join(dir, "unrecognized")
				}
				if kind == "malformed-request" {
					path = filepath.Join(dir, "forged.json")
				}
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "renamed" {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				// Simulate the durable rename after ReadDir and before inspection.
				if err := os.Rename(path, filepath.Join(root, "committed-fixture")); err != nil {
					t.Fatal(err)
				}
				if skip, err := capturePushAtomicTemp(dir, entries[0]); !skip || err != nil {
					t.Fatal("completed writer poisoned scan", err)
				}
				return
			}
			if _, err := drainCapturePush(context.Background(), &Container{}, root, root, "unused"); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("unsafe or malformed entry was ignored", err)
			}
		})
	}
}

func TestCapturePushPreparedTransportReusedForAppend(t *testing.T) {
	f := newCapturePushDriver(t)
	ctx := context.Background()
	p := f.attempt(t, "bound-append-push", domain.DocumentIdentityLegacy, true)
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	spy := capturePushSpy(t, f, p)
	prepared := 0
	f.c.Sync = capturePushForegroundFailure{push: func(inbound.SyncInput) error { t.Fatal("generic mutable Sync used"); return nil }}
	f.c.PrepareCapturePush = func(_ context.Context, cwd string, destination CapturePushDestination, repo string) (inbound.SyncRepo, error) {
		prepared++
		if prepared != 1 || cwd != f.cwd || destination != (CapturePushDestination{URL: capturePushTestContext}) || repo != f.repo.ID {
			t.Fatal("destination reselected")
		}
		return spy, nil
	}
	spy.before = func() {
		if len(spy.inputs) == 1 {
			spy.failure = domain.ErrSyncConflict
		} else {
			spy.failure = nil
		}
	}
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || !next.IsZero() {
		t.Fatal(next, err)
	}
	_, done, _ := readCapturePushRequest(t, f.cwd)
	if !done.Done || prepared != 1 || len(spy.inputs) != 2 || spy.inputs[0].Append || !spy.inputs[1].Append || spy.accepted != 1 {
		t.Fatal("append retry did not reuse one prepared transport")
	}
}

func TestCapturePushSameURLDifferentOriginModeRejected(t *testing.T) {
	f := newCapturePushDriver(t)
	ctx := context.Background()
	p := f.attempt(t, "origin-mode-push", domain.DocumentIdentityLegacy, true)
	if err := processFrozenCapture(ctx, f.c, f.cwd, f.cwd, p); err != nil {
		t.Fatal(err)
	}
	const api = "https://example.invalid/api/v1"
	t.Setenv("CXT_REMOTE", api)
	if err := providerfs.WriteRepoFileDurable(f.cwd, ".cxt/config", []byte(`{"remotes":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	updates, scope := capturePushSelection(p)
	if err := queueCapturePush(ctx, f.c, f.cwd, updates, scope); err != nil {
		t.Fatal(err)
	}
	_, before, raw := readCapturePushRequest(t, f.cwd)
	changed := before.Intent
	changed.ContextAPIOnly = false
	if !before.Intent.ContextAPIOnly || !bytes.Contains(raw, []byte(`"ContextAPIOnly":true`)) || changed.id() == before.Intent.id() {
		t.Fatal("destination kind was not durably bound into the intent identity")
	}
	// This string is also syntactically a two-segment repository URL. Changing
	// only its source must not silently reinterpret the original API intent.
	writeCapturePushOrigin(t, f.cwd, api)
	f.c.PrepareCapturePush = func(context.Context, string, CapturePushDestination, string) (inbound.SyncRepo, error) {
		t.Fatal("mode drift reached the remote factory")
		return nil, nil
	}
	if next, err := drainCapturePush(ctx, f.c, f.cwd, f.cwd, f.repo.ID); err != nil || next.IsZero() {
		t.Fatal("mode drift did not retain bounded retry", next, err)
	}
	_, after, _ := readCapturePushRequest(t, f.cwd)
	if after.Done || after.Tries != 1 || after.Intent != before.Intent || !strings.Contains(after.Error, "push destination changed") {
		t.Fatal("mode drift changed or acknowledged the immutable request")
	}
}
