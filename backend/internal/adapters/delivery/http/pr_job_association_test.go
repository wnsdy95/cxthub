package http

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRJobAssociationOpenAPIFields(t *testing.T) {
	properties := schemaProps(t, "../../../../../schemas/openapi.yaml", "PRJobAssociation")
	fields := jsonFields(reflect.TypeOf(domain.PRJobAssociation{}))
	if len(properties) != len(fields) {
		t.Fatal("association field count drift", properties, fields)
	}
	for _, field := range fields {
		if !properties[field] {
			t.Fatal("missing association field", field)
		}
	}
}
