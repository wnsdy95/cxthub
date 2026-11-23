package domain

import (
	"fmt"
)

type legacyNativeNestedFenceField struct {
	name   string
	kind   string
	prefix string
}

func (field legacyNativeNestedFenceField) child(value string) string {
	return field.prefix + value + `}`
}

func (field legacyNativeNestedFenceField) event(child string) string {
	return fmt.Sprintf(`{"kind":%q,%q:%s}`, field.kind, field.name, child)
}
