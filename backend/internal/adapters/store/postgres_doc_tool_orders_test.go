//go:build postgres

package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docToolOrdersPGTrace struct {
	pgx.Tx
	queries   []string
	arguments [][]any
	rows      int
	objects   int
}

type docToolOrdersPGRows struct {
	pgx.Rows
	trace *docToolOrdersPGTrace
}

func (rows *docToolOrdersPGRows) Next() bool {
	if rows.Rows.Next() {
		rows.trace.rows++
		return true
	}
	return false
}

func (trace *docToolOrdersPGTrace) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &docToolOrdersPGRows{Rows: rows, trace: trace}, nil
}

func (trace *docToolOrdersPGTrace) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	if strings.Contains(query, "doc_display_objects_v1") {
		trace.objects++
	}
	return trace.Tx.QueryRow(ctx, query, arguments...)
}

func docToolOrdersPGPlanArguments(test *testing.T, arguments []any) string {
	test.Helper()
	literals := make([]string, len(arguments))
	for index, argument := range arguments {
		switch value := argument.(type) {
		case domain.ContentHash:
			literals[index] = "'" + strings.ReplaceAll(string(value), "'", "''") + "'"
		case string:
			literals[index] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
		case []byte:
			literals[index] = "pg_catalog.decode('" + hex.EncodeToString(value) + "','hex')"
		case []string:
			if value == nil {
				literals[index] = "NULL::text[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::text[]"
		case [][]byte:
			if value == nil {
				literals[index] = "NULL::bytea[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::bytea[]"
		case []int64:
			if value == nil {
				literals[index] = "NULL::bigint[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::bigint[]"
		case int:
			literals[index] = fmt.Sprint(value)
		case int64:
			literals[index] = fmt.Sprint(value)
		case bool:
			literals[index] = fmt.Sprint(value)
		default:
			test.Fatalf("unsupported traced plan argument %T", argument)
		}
	}
	return strings.Join(literals, ",")
}

func explainDocToolOrdersPGTraceQuery(test *testing.T, read context.Context, transaction pgx.Tx, query string, arguments []any, mode string) []byte {
	test.Helper()
	if mode != "force_custom_plan" && mode != "force_generic_plan" {
		test.Fatal("invalid plan cache mode", mode)
	}
	if _, err := transaction.Exec(read, `SET LOCAL plan_cache_mode=`+mode); err != nil {
		test.Fatal(err)
	}
	statement := domain.NewID("orders_plan_")
	name := pgx.Identifier{statement}.Sanitize()
	if _, err := transaction.Exec(read, `PREPARE `+name+` AS `+query); err != nil {
		test.Fatal("prepare actual traced query", mode, query, err)
	}
	defer func() {
		if _, err := transaction.Exec(read, `DEALLOCATE `+name); err != nil {
			test.Error("deallocate traced query", err)
		}
	}()
	var encoded []byte
	if err := transaction.QueryRow(read, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON,COSTS OFF) EXECUTE `+name+`(`+
		docToolOrdersPGPlanArguments(test, arguments)+`)`).Scan(&encoded); err != nil {
		test.Fatal("explain actual traced query", mode, query, err)
	}
	var generic, custom int64
	if err := transaction.QueryRow(read, `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, statement).Scan(&generic, &custom); err != nil {
		test.Fatal("verify traced statement plan mode", mode, err)
	}
	if mode == "force_custom_plan" && (custom != 1 || generic != 0) || mode == "force_generic_plan" && (generic != 1 || custom != 0) {
		test.Fatal("traced statement used wrong plan mode", mode, generic, custom)
	}
	return encoded
}
