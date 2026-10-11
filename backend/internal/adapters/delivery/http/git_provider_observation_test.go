package http

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitProviderObservationOpenAPIFields(t *testing.T) {
	for _, testcase := range []struct {
		schema string
		model  reflect.Type
	}{
		{"GitRefObservation", reflect.TypeOf(domain.GitRefObservation{})},
		{"GitProviderObservation", reflect.TypeOf(domain.GitProviderObservation{})},
		{"GitPullRequestObservation", reflect.TypeOf(domain.GitPullRequestObservation{})},
	} {
		t.Run(testcase.schema, func(t *testing.T) {
			properties := schemaProps(t, "../../../../../schemas/openapi.yaml", testcase.schema)
			fields := jsonFields(testcase.model)
			if len(properties) != len(fields) {
				t.Fatal("field count drift", properties, fields)
			}
			for _, field := range fields {
				if !properties[field] {
					t.Fatal("missing observation field", field)
				}
			}
		})
	}
}
