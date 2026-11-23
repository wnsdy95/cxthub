package app

func nonNil[Value any](items []Value) []Value {
	if items == nil {
		return []Value{}
	}
	return items
}
