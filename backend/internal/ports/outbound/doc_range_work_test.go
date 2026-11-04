package outbound

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocRangeWorkCapabilitiesRemainJobBound(tests *testing.T) {
	for _, contract := range []struct {
		name    string
		port    reflect.Type
		methods map[string]reflect.Type
	}{
		{
			name: "store",
			port: reflect.TypeFor[DocRangeWorkStore](),
			methods: map[string]reflect.Type{
				"PrepareDocRangeWork":           reflect.TypeFor[func(context.Context, domain.DocRangeIndexJob) (PreparedDocRangeWork, error)](),
				"BuildDocRangeCompatibilityJob": reflect.TypeFor[func(context.Context, domain.DocRangeIndexJob) error](),
			},
		},
		{
			name: "prepared",
			port: reflect.TypeFor[PreparedDocRangeWork](),
			methods: map[string]reflect.Type{
				"Step":              reflect.TypeFor[func(context.Context, domain.DocRangeIndexJob) (DocRangeWorkProgress, error)](),
				"Publish":           reflect.TypeFor[func(context.Context, domain.DocRangeIndexJob) error](),
				"RetainedResources": reflect.TypeFor[func() (RootPublicationResources, error)](),
				"Close":             reflect.TypeFor[func() error](),
			},
		},
	} {
		tests.Run(contract.name, func(test *testing.T) {
			if contract.port.NumMethod() != len(contract.methods) {
				test.Fatalf("capability surface changed: got %d methods, want %d", contract.port.NumMethod(), len(contract.methods))
			}
			for name, signature := range contract.methods {
				method, found := contract.port.MethodByName(name)
				if !found || method.Type != signature {
					test.Fatalf("%s signature changed: got %v, want %v", name, method.Type, signature)
				}
			}
		})
	}
}

func TestDocRangeWorkProgressHasExplicitNonzeroOutcomes(test *testing.T) {
	if reflect.TypeFor[DocRangeWorkProgress]().Kind() != reflect.Uint8 ||
		DocRangeWorkPending != 1 || DocRangeWorkPublishable != 2 || DocRangeWorkCompatibilityRequired != 3 {
		test.Fatal("document range progress changed its explicit nonzero uint8 protocol")
	}
}
