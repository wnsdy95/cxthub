package domain

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type graphProjectionSchemaCacheRow struct {
	Name     string `json:"name"`
	Counter  uint64 `json:"counter,string"`
	Optional string `json:"optional,omitempty"`
}

func TestGraphProjectionSchemaCacheKeepsObjectValidationLocal(t *testing.T) {
	ctx := context.Background()
	valid := []byte(`{"value":[{"name":"first","counter":"1"},{"counter":"2","name":"second","optional":"kept"}]}`)
	want := []graphProjectionSchemaCacheRow{{Name: "first", Counter: 1}, {Name: "second", Counter: 2, Optional: "kept"}}
	decode := func(raw []byte) ([]graphProjectionSchemaCacheRow, error) {
		var rows []graphProjectionSchemaCacheRow
		field := graphProjectionTypedField("rows", &rows)
		budget := graphProjectionBudget{ctx: ctx}
		err := field.decode(ctx, raw, &budget)
		return rows, err
	}
	for _, raw := range []string{
		`{"value":[{"name":"first","counter":"1"},{"name":"second"}]}`,
		`{"value":[{"counter":"2"}]}`,
		`{"value":[{"name":"first","name":"second","counter":"1"}]}`,
		`{"value":[{"name":"first","counter":"1","optional":"a","optional":"b"}]}`,
		`{"value":[{"name":"first","counter":"1","NAME":"alias"}]}`,
		`{"value":[{"name":"first","counter":"1","unknown":true}]}`,
		`{"value":[{"name":"first","counter":1}]}`,
		`{"value":[{"name":"first","counter":null}]}`,
		`{"value":[null]}`,
		`{"value":[],"value":[]}`,
	} {
		if _, err := decode(valid); err != nil {
			t.Fatal("valid object did not warm schema cache", err)
		}
		if _, err := decode([]byte(raw)); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("cached schema relaxed validation for %s: %v", raw, err)
		}
		var got []graphProjectionSchemaCacheRow
		budget := graphProjectionBudget{ctx: ctx}
		if err := graphProjectionTypedField("rows", &got).decode(ctx, valid, &budget); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("failed or previous decode changed cached schema", got, err)
		}
	}
	budget := graphProjectionBudget{ctx: ctx, rows: MaxPendingProjectionOutputRows - 1}
	var got []graphProjectionSchemaCacheRow
	if err := graphProjectionTypedField("rows", &got).decode(ctx, valid, &budget); !errors.Is(err, ErrRepositoryViewSourceLimit) || got != nil {
		t.Fatal("warm schema bypassed per-read nested row budget", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	budget = graphProjectionBudget{ctx: canceled}
	if err := graphProjectionTypedField("rows", &got).decode(canceled, valid, &budget); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatal("warm schema bypassed cancellation", got, err)
	}
}

func TestGraphProjectionSchemaCacheDoesNotShareMutableFieldMaps(t *testing.T) {
	schema := reflect.TypeFor[graphViewProjectionSnapshot]()
	compiled, err := graphProjectionJSONStruct(schema)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := graphProjectionJSONFields(schema)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]int(nil), compiled.fields["repo_id"].definition.Index...)
	fields["repo_id"].Index[0] = -1
	delete(fields, "branches")
	again, err := graphProjectionJSONStruct(schema)
	if err != nil || again != compiled || len(again.fields) != len(fields)+1 || !reflect.DeepEqual(again.fields["repo_id"].definition.Index, original) {
		t.Fatal("mutable field discovery changed immutable schema", err)
	}
	for _, key := range []string{"branches", "graft_parents", "models"} {
		field := compiled.fields[key]
		if !field.required || field.quoted || len(field.definition.Index) != 1 {
			t.Fatal("embedded override metadata changed", key, field)
		}
	}
	positions := make(map[int]bool, len(compiled.fields))
	required := 0
	for _, field := range compiled.fields {
		if field.position < 0 || field.position >= len(compiled.fields) || positions[field.position] {
			t.Fatal("invalid per-object seen index", field.position)
		}
		positions[field.position] = true
		if field.required {
			required++
		}
	}
	if required != compiled.required {
		t.Fatal("compiled required count changed")
	}
}

func TestGraphProjectionSchemaCacheConcurrentFirstUse(t *testing.T) {
	type concurrentRow struct {
		ID       string `json:"id"`
		Sequence uint64 `json:"sequence,string"`
		Optional bool   `json:"optional,omitempty"`
	}
	const workersCount = 24
	compiled := make([]*graphProjectionJSONStructSchema, workersCount)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := range workersCount {
		workers.Go(func() {
			<-start
			var err error
			compiled[worker], err = graphProjectionJSONStruct(reflect.TypeFor[concurrentRow]())
			if err != nil {
				t.Error(err)
				return
			}
			for iteration := range 10 {
				var rows []concurrentRow
				field := graphProjectionTypedField("rows", &rows)
				budget := graphProjectionBudget{ctx: context.Background()}
				if iteration%2 == 0 {
					if err := field.decode(budget.ctx, []byte(`{"value":[{"id":"first","sequence":"1"},{"id":"second","sequence":"2","optional":true}]}`), &budget); err != nil || len(rows) != 2 || !rows[1].Optional {
						t.Error("concurrent cache validation", err)
					}
				} else if err := field.decode(budget.ctx, []byte(`{"value":[{"id":"first","id":"second","sequence":"1"}]}`), &budget); !errors.Is(err, ErrIntegrity) || rows != nil {
					t.Error("concurrent duplicate rejection", err)
				}
			}
		})
	}
	close(start)
	workers.Wait()
	for _, result := range compiled {
		if result == nil || result != compiled[0] {
			t.Fatal("concurrent cache publication returned different schemas")
		}
	}
}
