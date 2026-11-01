package serverruntime

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func requirePRCoordinationRuntime(test *testing.T, condition bool, details ...any) {
	test.Helper()
	if !condition {
		test.Fatal(details...)
	}
}

func TestPRCoordinationRuntimeActualOwnerCapturedWorkAndFS(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer runtime.Close()
	originalStore, actualFS := runtime.Store.(*store.FSStore)
	if !actualFS {
		test.Fatalf("PR fixture is not actual FS: %T", runtime.Store)
	}
	if owner, actual := runtime.PRPromotions.(*app.PRPromotionCommands); !actual || owner == nil {
		test.Fatalf("API omitted actual PR owner: %T", runtime.PRPromotions)
	}
	requirePRCoordinationRuntime(test, runtime.ConfigurePRPromotionCommands() == nil, "configure actual PR owner")
	owner, actual := runtime.PRPromotions.(*app.PRPromotionCommands)
	if !actual || owner == nil {
		test.Fatalf("configured PR facade: %T", runtime.PRPromotions)
	}
	factory, err := runtime.Context.PRPromotionCommands()
	factoryOwner, actual := factory.(*app.PRPromotionCommands)
	if err != nil || !actual || factoryOwner == nil {
		test.Fatalf("factory omitted actual owner: %T %v", factory, err)
	}
	for _, commands := range []*app.PRPromotionCommands{owner, factoryOwner} {
		value := reflect.ValueOf(commands).Elem()
		scope := value.FieldByName("scope")
		if scope.Kind() == reflect.Interface {
			scope = scope.Elem()
		}
		requirePRCoordinationRuntime(test, !(scope.Type().String() != "*app.prCoordinationScope" || scope.IsNil()), "PR scope is not actual passed-work scope", scope.Type())
		work := scope.Elem().FieldByName("work")
		requirePRCoordinationRuntime(test, !(work.IsNil() || work.Elem().FieldByName("meta").Elem().Pointer() != reflect.ValueOf(originalStore).Pointer()), "PR scope lost original store")
		for field, expectedType := range map[string]string{"submission": "*app.prPublicationCommands", "executor": "*app.PRPromotionWorker"} {
			dependency := value.FieldByName(field)
			if dependency.Kind() == reflect.Interface {
				dependency = dependency.Elem()
			}
			requirePRCoordinationRuntime(test, !(dependency.Type().String() != expectedType || dependency.IsNil() || dependency.Elem().FieldByName("work").Pointer() != work.Pointer()), "PR owner replaced actual policy or shared work", field, dependency.Type())
		}
	}
	ctx := inbound.WithSystemActor(test.Context())
	repo := domain.HashContent([]byte(test.Name()))
	_, err = originalStore.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/project.git"})
	requirePRCoordinationRuntime(test, err == nil, err)
	request := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature/nested", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	service := runtime.Context
	reflect.ValueOf(service).Elem().SetZero()
	runtime.Context = nil
	for _, commands := range []*app.PRPromotionCommands{owner, factoryOwner} {
		job, err := commands.SubmitPRPromotion(ctx, repo, request)
		requirePRCoordinationRuntime(test, !(err != nil || job.ID != domain.PRPromotionID(repo, request.Number) || job.Diagnostics == nil), "actual submission lost captured FS work", job, err)
		rows, err := commands.ListPRPromotions(ctx, repo)
		requirePRCoordinationRuntime(test, !(err != nil || len(rows) != 1 || rows[0].ID != job.ID || rows[0].Association == nil || rows[0].Association.CurrentApplicability != "not_evaluated"), "actual list lost original queue or projection", rows, err)
		requirePRCoordinationRuntime(test, commands.RetryPRPromotion(ctx, repo, job.ID) == nil, "actual retry retained Service")
	}
	_, releaseBulk, err := app.AcquireResources(ctx, app.ResourceBulk)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer releaseBulk()
	interactive, releaseInteractive, err := app.AcquireResources(ctx, app.ResourceInteractive)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer releaseInteractive()
	request.Number++
	output, err := owner.DeliverPRPromotion(interactive, repo, request)
	requirePRCoordinationRuntime(test, !(!errors.Is(err, domain.ErrResourceOverloaded) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{})), "Deliver did not defer Bulk until after acceptance", output, err)
	accepted, err := originalStore.GetPRJob(ctx, repo, domain.PRPromotionID(repo, request.Number))
	requirePRCoordinationRuntime(test, !(err != nil || accepted.PR != request || accepted.State != "waiting"), "Bulk rejection lost durable submission", accepted, err)
	runtime.Context, runtime.PRPromotions = service, owner
	err = runtime.ConfigurePRPromotionCommands()
	requirePRCoordinationRuntime(test, !(err == nil || runtime.PRPromotions != nil), "failed factory retained stale PR owner", err)
}

func TestPRCoordinationRuntimeMissingContextClearsStaleOwner(test *testing.T) {
	for _, runtime := range []*Runtime{nil, {}} {
		if runtime != nil {
			runtime.PRPromotions = &app.PRPromotionCommands{}
		}
		err := runtime.ConfigurePRPromotionCommands()
		requirePRCoordinationRuntime(test, !(!errors.Is(err, domain.ErrCatalogUnsupported) || runtime != nil && runtime.PRPromotions != nil), "missing Context retained PR owner", err)
	}
}

func TestPRCoordinationRuntimeMCPHasNoOwner(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer runtime.Close()
	requirePRCoordinationRuntime(test, !(runtime.Context != nil || runtime.PRPromotions != nil), "OpenMCP assembled PR commands")
	runtime.PRPromotions = &app.PRPromotionCommands{}
	err = runtime.ConfigurePRPromotionCommands()
	requirePRCoordinationRuntime(test, !(!errors.Is(err, domain.ErrCatalogUnsupported) || runtime.PRPromotions != nil), "read-only runtime retained PR commands", err)
}
