package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/webhookdelivery"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

type apiLifecycleContextKey struct{}

func waitAPILifecycleSignal(test *testing.T, signal <-chan struct{}, description string) {
	test.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		test.Fatalf("timed out waiting for %s", description)
	}
}

func TestAPILifecycleCloseCancelsAndJoinsBeforeRuntimeClose(test *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	stopped := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	lifecycle := newAPILifecycle(parent, func() {
		select {
		case <-exited:
		default:
			test.Error("runtime closed while a worker was still live")
		}
		close(closed)
	})
	test.Cleanup(func() {
		unblock()
		lifecycle.close()
	})
	lifecycle.start(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		close(exited)
	})
	waitAPILifecycleSignal(test, started, "worker startup")
	go func() {
		lifecycle.close()
		close(stopped)
	}()
	waitAPILifecycleSignal(test, canceled, "worker cancellation")
	select {
	case <-closed:
		test.Error("runtime closed before the pending worker was released")
	default:
	}
	select {
	case <-stopped:
		test.Error("lifecycle returned before the pending worker was released")
	default:
	}
	if parent.Err() != nil {
		test.Error("lifecycle canceled its caller's context")
	}
	unblock()
	waitAPILifecycleSignal(test, stopped, "joined lifecycle shutdown")
	waitAPILifecycleSignal(test, closed, "runtime close after worker exit")
	lifecycle.close()
}

func TestAPILifecycleListenerFailureJoinsBeforeReturningOriginalError(test *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	listenerErrors := make(chan error, 1)
	returned := make(chan error, 1)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	lifecycle := newAPILifecycle(context.Background(), func() {
		select {
		case <-exited:
		default:
			test.Error("listener failure closed the runtime before worker exit")
		}
		close(closed)
	})
	test.Cleanup(func() {
		unblock()
		lifecycle.close()
	})
	lifecycle.start(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		close(exited)
	})
	waitAPILifecycleSignal(test, started, "worker startup before listener failure")
	go func() {
		returned <- func() error {
			defer lifecycle.close()
			err := serverruntime.Serve(lifecycle.ctx, "127.0.0.1:-1", http.NotFoundHandler())
			listenerErrors <- err
			return err
		}()
	}()
	var listenerErr error
	select {
	case listenerErr = <-listenerErrors:
		if listenerErr == nil {
			test.Fatal("invalid listener address unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		test.Fatal("listener failure did not return")
	}
	waitAPILifecycleSignal(test, canceled, "worker cancellation after listener failure")
	select {
	case err := <-returned:
		test.Fatalf("listener error escaped before worker join: %v", err)
	default:
	}
	select {
	case <-closed:
		test.Error("runtime closed before listener-failure drain completed")
	default:
	}
	unblock()
	select {
	case err := <-returned:
		if err != listenerErr {
			test.Fatalf("listener error changed during shutdown: got %v, want %v", err, listenerErr)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("joined listener failure did not return")
	}
	waitAPILifecycleSignal(test, closed, "runtime close after listener-failure join")
}

func TestAPILifecycleDeferredCloseJoinsOnPanic(test *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	panicValue := errors.New("startup panic")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		lifecycle := newAPILifecycle(context.Background(), func() {
			select {
			case <-exited:
			default:
				test.Error("panic cleanup closed the runtime before worker exit")
			}
			close(closed)
		})
		defer lifecycle.close()
		lifecycle.start(func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			close(exited)
		})
		waitAPILifecycleSignal(test, started, "worker startup before panic")
		panic(panicValue)
	}()
	if recovered != panicValue {
		test.Fatalf("panic changed during lifecycle cleanup: got %v, want %v", recovered, panicValue)
	}
	waitAPILifecycleSignal(test, closed, "runtime close during panic cleanup")
}

type apiLifecycleMaintenanceStore struct{}

func (apiLifecycleMaintenanceStore) StepDocDisplayMaintenance(context.Context) (domain.DocDisplayMaintenanceStep, error) {
	return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
}

type apiLifecycleDocumentStore struct {
	outbound.DocFindFieldsStore
	outbound.DocDisplayStore
}

type apiLifecycleRangeStore struct {
	*store.FSStore
	outbound.DocRangeJobStore
}

func (apiLifecycleRangeStore) ClaimDocRangeJob(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, domain.ErrNotFound
}

func (apiLifecycleDocumentStore) ClaimDocFindFields(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, domain.ErrNotFound
}

func (apiLifecycleDocumentStore) ClaimDocDisplay(context.Context, time.Time, time.Duration) (domain.DocDisplayJob, error) {
	return domain.DocDisplayJob{}, domain.ErrNotFound
}

func TestAPILifecycleJoinsActualWorkersWithInheritedCancellation(test *testing.T) {
	test.Run("without GitHub", func(test *testing.T) {
		testAPILifecycleJoinsActualWorkers(test, false)
	})
	test.Run("with GitHub", func(test *testing.T) {
		testAPILifecycleJoinsActualWorkers(test, true)
	})
}

func testAPILifecycleJoinsActualWorkers(test *testing.T, withGitHub bool) {
	test.Helper()
	storage, err := store.OpenFSStore(test.TempDir())
	if err != nil {
		test.Fatal(err)
	}
	service := app.NewService(storage, apiLifecycleRangeStore{FSStore: storage}, nil, nil, storage)
	service.ConfigureDocumentFindFields(apiLifecycleDocumentStore{})
	service.ConfigureDocumentDisplay(apiLifecycleDocumentStore{})
	documentWorkers, err := service.DocumentWorkersForProfile(documentquery.LegacyCompatibility)
	if err != nil {
		test.Fatal(err)
	}
	if documentWorkers.Finalization == nil || documentWorkers.Range == nil || documentWorkers.FindFields == nil || documentWorkers.Display == nil {
		test.Fatal("actual API document owners are missing")
	}
	projectionWorkers, err := service.ProjectionWorkersForProfile(projectionworker.Compatibility)
	if err != nil {
		test.Fatal(err)
	}
	if projectionWorkers.Pending == nil || projectionWorkers.Graph == nil || projectionWorkers.Qualification == nil {
		test.Fatal("actual API projection owners are missing")
	}
	identity := app.NewIdentityService(nil, storage)
	invitationEmailWorker, err := app.NewInvitationEmailWorker(identity.InvitationEmailWorkerConfig(app.InvitationEmailCompatibility))
	if err != nil || invitationEmailWorker == nil {
		test.Fatal("actual invitation email owner is missing", err)
	}
	notificationWorker, err := app.NewNotificationWorker(app.NotificationWorkerConfig{
		Profile: notificationworker.Complete, Metadata: storage, Blobs: storage, Repositories: storage, Sender: webhookdelivery.New(),
	})
	if err != nil {
		test.Fatal(err)
	}
	maintenanceWorkers, err := app.NewMaintenanceWorkers(app.MaintenanceWorkersConfig{Profile: systemmaintenance.Compatibility, Repositories: storage})
	if err != nil || maintenanceWorkers.Runtime == nil || maintenanceWorkers.Storage == nil {
		test.Fatal("actual auxiliary maintenance owners are missing", err)
	}
	reader := gitevidence.NewGitHub(func() string { return "" })
	gitChangeWorker, err := app.NewGitChangeWorker(app.GitChangeWorkerConfig{
		Profile: gitchangeworker.Compatibility, Metadata: storage, Blobs: storage, Repositories: storage, Reader: reader,
	})
	if err != nil {
		test.Fatal(err)
	}
	gitIntegrationWorkers, err := app.NewGitIntegrationWorkers(app.GitIntegrationWorkerConfig{
		Profile: app.GitIntegrationCompatibility, Metadata: storage, Blobs: storage, Repositories: storage, Reader: reader,
	})
	if err != nil {
		test.Fatal(err)
	}
	memoryPreparations, err := app.NewMemoryPreparationWorkers(app.MemoryPreparationWorkerConfig{
		Profile: app.MemoryPreparationCompatibility, Resources: service.MemoryPreparationResources(),
	})
	if err != nil {
		test.Fatal(err)
	}
	prPromotionWorker, err := app.NewPRPromotionWorker(service.PRPromotionWorkerConfig(app.PRPromotionCompatibility))
	if err != nil {
		test.Fatal(err)
	}
	maintenance, err := app.NewDisplayMaintenanceWorker(apiLifecycleMaintenanceStore{})
	if err != nil {
		test.Fatal(err)
	}
	type workerBinding struct {
		name string
		run  func(context.Context)
	}
	var workers []workerBinding
	expectedWorkers := 17
	if withGitHub {
		github := app.NewGitHubConnections(identity, service, storage, nil)
		githubWorker, err := app.NewGitHubWorker(github.GitHubWorkerConfig(app.GitHubWorkerCompatibility))
		if err != nil || githubWorker == nil {
			test.Fatal("actual GitHub synchronization owner is missing", err)
		}
		workers = append(workers, workerBinding{name: "github", run: githubWorker.Run})
		expectedWorkers++
	}
	workers = append(workers, []workerBinding{
		{name: "git changes", run: gitChangeWorker.Run},
		{name: "git scans", run: gitIntegrationWorkers.Run},
		{name: "git reconciliation", run: gitIntegrationWorkers.RunReconciler},
		{name: "document finalization", run: documentWorkers.Finalization.Run},
		{name: "document range", run: documentWorkers.Range.Run},
		{name: "document find fields", run: documentWorkers.FindFields.Run},
		{name: "document display", run: documentWorkers.Display.Run},
		{name: "document display maintenance", run: maintenance.Run},
		{name: "pending projection", run: projectionWorkers.Pending.Run},
		{name: "graph projection", run: projectionWorkers.Graph.Run},
		{name: "memory preparation", run: memoryPreparations.Run},
		{name: "graph qualification", run: projectionWorkers.Qualification.Run},
		{name: "PR promotion", run: prPromotionWorker.Run},
		{name: "notifications", run: notificationWorker.Run},
		{name: "invitation email", run: invitationEmailWorker.Run},
		{name: "runtime maintenance", run: maintenanceWorkers.Runtime.Run},
		{name: "storage maintenance", run: maintenanceWorkers.Storage.Run},
	}...)
	if len(workers) != expectedWorkers {
		test.Fatalf("actual API worker count: got %d, want %d", len(workers), expectedWorkers)
	}
	parent, cancelParent := context.WithTimeout(context.WithValue(context.Background(), apiLifecycleContextKey{}, "inherited"), time.Minute)
	defer cancelParent()
	entered := make(chan context.Context, len(workers))
	returned := make(chan string, len(workers))
	closed := make(chan struct{})
	lifecycle := newAPILifecycle(parent, func() {
		if len(returned) != len(workers) {
			test.Errorf("runtime closed with only %d/%d workers returned", len(returned), len(workers))
		}
		close(closed)
	})
	test.Cleanup(lifecycle.close)
	for _, worker := range workers {
		lifecycle.start(func(ctx context.Context) {
			entered <- ctx
			worker.run(ctx)
			returned <- worker.name
		})
	}
	for range workers {
		select {
		case workerContext := <-entered:
			if workerContext != lifecycle.ctx || workerContext.Value(apiLifecycleContextKey{}) != "inherited" {
				test.Error("worker did not inherit the single API lifecycle context")
			}
			deadline, hasDeadline := workerContext.Deadline()
			parentDeadline, _ := parent.Deadline()
			if !hasDeadline || !deadline.Equal(parentDeadline) {
				test.Error("worker lost the caller's deadline")
			}
		case <-time.After(5 * time.Second):
			test.Fatal("an actual worker did not enter its lifecycle")
		}
	}
	cancelParent()
	waitAPILifecycleSignal(test, lifecycle.ctx.Done(), "inherited parent cancellation")
	stopped := make(chan struct{})
	go func() {
		lifecycle.close()
		close(stopped)
	}()
	waitAPILifecycleSignal(test, stopped, "all actual worker returns")
	waitAPILifecycleSignal(test, closed, "runtime close after all actual worker returns")
	seen := make(map[string]bool, len(workers))
	for range workers {
		name := <-returned
		if seen[name] {
			test.Errorf("worker returned more than once: %s", name)
		}
		seen[name] = true
	}
	if !errors.Is(lifecycle.ctx.Err(), context.Canceled) {
		test.Fatalf("worker context was not canceled: %v", lifecycle.ctx.Err())
	}
}

type apiLifecycleGitHubRemote struct {
	outbound.GitHubApp
	started  chan<- context.Context
	canceled chan<- struct{}
	release  <-chan struct{}
}

func (remote apiLifecycleGitHubRemote) Installation(ctx context.Context, _ int64) (domain.GitHubInstallation, error) {
	remote.started <- ctx
	<-ctx.Done()
	close(remote.canceled)
	<-remote.release
	return domain.GitHubInstallation{}, ctx.Err()
}

func TestAPILifecycleJoinsActualGitHubWorkerBeforeRuntimeClose(test *testing.T) {
	storage, err := store.OpenFSStore(test.TempDir())
	if err != nil {
		test.Fatal(err)
	}
	identity := app.NewIdentityService(nil, storage)
	owner := domain.User{ID: "lifecycle-github-owner", Username: "lifecycle-github-owner", Email: "owner@example.test", Name: "Lifecycle Owner"}
	if err := storage.UpsertUser(context.Background(), owner); err != nil {
		test.Fatal(err)
	}
	organization, err := identity.CreateOrganization(context.Background(), owner, "Lifecycle GitHub", "lifecycle-github")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.PutGitHubConnection(context.Background(), domain.GitHubConnection{
		NamespaceID:  organization.NamespaceID,
		Installation: domain.GitHubInstallation{ID: 789, AccountID: 456, Login: "lifecycle-github", Kind: "Organization"},
		Generation:   1, Enabled: true, Status: "pending", UpdatedBy: owner.ID,
	}); err != nil {
		test.Fatal(err)
	}
	started := make(chan context.Context, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	stopped := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	remote := apiLifecycleGitHubRemote{started: started, canceled: canceled, release: release}
	service := app.NewService(storage, storage, nil, nil, storage)
	github := app.NewGitHubConnections(identity, service, storage, remote)
	githubWorker, err := app.NewGitHubWorker(github.GitHubWorkerConfig(app.GitHubWorkerCompatibility))
	if err != nil || githubWorker == nil {
		test.Fatal("actual GitHub synchronization owner is missing", err)
	}
	parent, cancelParent := context.WithTimeout(context.WithValue(context.Background(), apiLifecycleContextKey{}, "inherited"), 3*time.Minute)
	defer cancelParent()
	lifecycle := newAPILifecycle(parent, func() {
		select {
		case <-exited:
		default:
			test.Error("runtime closed before the actual GitHub synchronization owner returned")
		}
		close(closed)
	})
	test.Cleanup(func() {
		unblock()
		lifecycle.close()
	})
	lifecycle.start(func(ctx context.Context) {
		githubWorker.Run(ctx)
		close(exited)
	})
	select {
	case installationContext := <-started:
		if installationContext.Value(apiLifecycleContextKey{}) != "inherited" {
			test.Error("GitHub synchronization lost the API lifecycle context")
		}
		deadline, hasDeadline := installationContext.Deadline()
		parentDeadline, _ := parent.Deadline()
		if !hasDeadline || !deadline.Before(parentDeadline) || time.Until(deadline) > 90*time.Second {
			test.Error("GitHub synchronization lost its bounded tick deadline")
		}
	case <-time.After(5 * time.Second):
		test.Fatal("actual GitHub synchronization owner did not reach the fake remote")
	}
	go func() {
		lifecycle.close()
		close(stopped)
	}()
	waitAPILifecycleSignal(test, canceled, "actual GitHub synchronization cancellation")
	select {
	case <-closed:
		test.Error("runtime closed while the fake GitHub remote was still live")
	default:
	}
	select {
	case <-stopped:
		test.Error("lifecycle returned before the actual GitHub synchronization owner joined")
	default:
	}
	if parent.Err() != nil {
		test.Error("GitHub synchronization lifecycle canceled its caller's context")
	}
	unblock()
	waitAPILifecycleSignal(test, stopped, "actual GitHub synchronization owner join")
	waitAPILifecycleSignal(test, closed, "runtime close after actual GitHub synchronization owner join")
}

type apiLifecycleInvitationMailer func(context.Context, string, domain.EmailMessage) (string, error)

func (sender apiLifecycleInvitationMailer) Send(ctx context.Context, key string, message domain.EmailMessage) (string, error) {
	return sender(ctx, key, message)
}

func TestAPILifecycleJoinsActualInvitationEmailBeforeRuntimeClose(test *testing.T) {
	storage, err := store.OpenFSStore(test.TempDir())
	if err != nil {
		test.Fatal(err)
	}
	identity := app.NewIdentityService(nil, storage)
	owner := domain.User{ID: "lifecycle-email-owner", Username: "lifecycle-email-owner", Email: "owner@example.test", Name: "Lifecycle Owner"}
	if err := storage.UpsertUser(context.Background(), owner); err != nil {
		test.Fatal(err)
	}
	organization, err := identity.CreateOrganization(context.Background(), owner, "Lifecycle Email", "lifecycle-email")
	if err != nil {
		test.Fatal(err)
	}
	started := make(chan context.Context, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	closed := make(chan struct{})
	stopped := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	sender := apiLifecycleInvitationMailer(func(ctx context.Context, _ string, _ domain.EmailMessage) (string, error) {
		started <- ctx
		<-ctx.Done()
		close(canceled)
		<-release
		return "", ctx.Err()
	})
	if err := identity.ConfigureInvitationEmail(sender, "from@example.test", "https://example.test"); err != nil {
		test.Fatal(err)
	}
	if _, err := identity.CreateCollaborationInvitation(context.Background(), owner.ID, "organization", organization.ID, "recipient@example.test", domain.OrganizationMember, 7); err != nil {
		test.Fatal(err)
	}
	invitationEmailWorker, err := app.NewInvitationEmailWorker(identity.InvitationEmailWorkerConfig(app.InvitationEmailCompatibility))
	if err != nil || invitationEmailWorker == nil {
		test.Fatal("actual invitation email owner is missing", err)
	}
	parent, cancelParent := context.WithTimeout(context.WithValue(context.Background(), apiLifecycleContextKey{}, "inherited"), time.Minute)
	defer cancelParent()
	lifecycle := newAPILifecycle(parent, func() {
		select {
		case <-exited:
		default:
			test.Error("runtime closed before the actual invitation email owner returned")
		}
		close(closed)
	})
	test.Cleanup(func() {
		unblock()
		lifecycle.close()
	})
	lifecycle.start(func(ctx context.Context) {
		invitationEmailWorker.Run(ctx)
		close(exited)
	})
	select {
	case sendContext := <-started:
		if sendContext.Value(apiLifecycleContextKey{}) != "inherited" {
			test.Error("invitation email send lost the API lifecycle context")
		}
		deadline, hasDeadline := sendContext.Deadline()
		parentDeadline, _ := parent.Deadline()
		if !hasDeadline || !deadline.Before(parentDeadline) {
			test.Error("invitation email send lost its bounded request deadline")
		}
	case <-time.After(5 * time.Second):
		test.Fatal("actual invitation email owner did not reach the fake sender")
	}
	go func() {
		lifecycle.close()
		close(stopped)
	}()
	waitAPILifecycleSignal(test, canceled, "actual invitation email send cancellation")
	select {
	case <-closed:
		test.Error("runtime closed while the fake invitation email sender was still live")
	default:
	}
	select {
	case <-stopped:
		test.Error("lifecycle returned before the actual invitation email owner joined")
	default:
	}
	if parent.Err() != nil {
		test.Error("invitation email lifecycle canceled its caller's context")
	}
	unblock()
	waitAPILifecycleSignal(test, stopped, "actual invitation email owner join")
	waitAPILifecycleSignal(test, closed, "runtime close after actual invitation email owner join")
}

func TestAPILifecycleClosesWithoutStartedWorkers(test *testing.T) {
	closed := 0
	lifecycle := newAPILifecycle(context.Background(), func() { closed++ })
	lifecycle.close()
	lifecycle.close()
	if closed != 1 || !errors.Is(lifecycle.ctx.Err(), context.Canceled) {
		test.Fatalf("early startup cleanup: close count=%d, context=%v", closed, lifecycle.ctx.Err())
	}
}
