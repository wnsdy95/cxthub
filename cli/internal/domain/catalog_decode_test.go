package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogDecodeStrictEnvelope(t *testing.T) {
	page := catalogTestPage(catalogTestProtocol(t, 1), catalogTestSnapshotEntry(t, "snapshot", 1))
	raw := string(catalogTestRaw(t, page))
	if _, err := DecodeCatalogPage([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"null", `null`},
		{"array", `[]`},
		{"string", `"page"`},
		{"trailing_object", raw + `{}`},
		{"trailing_null", raw + `null`},
		{"trailing_garbage", raw + `x`},
		{"unknown", `{"future":true,` + raw[1:]},
		{"duplicate_version", `{"version":1,` + raw[1:]},
		{"escaped_duplicate", `{"\u0076ersion":1,` + raw[1:]},
		{"case_alias", strings.Replace(raw, `"version":`, `"Version":`, 1)},
		{"string_version", strings.Replace(raw, `"version":1`, `"version":"1"`, 1)},
		{"null_version", strings.Replace(raw, `"version":1`, `"version":null`, 1)},
		{"fractional_version", strings.Replace(raw, `"version":1`, `"version":1.0`, 1)},
		{"overflow_sequence", strings.Replace(raw, `"sequence":0`, `"sequence":9223372036854775808`, 1)},
		{"string_sequence", strings.Replace(raw, `"sequence":0`, `"sequence":"0"`, 1)},
		{"null_sequence", strings.Replace(raw, `"sequence":0`, `"sequence":null`, 1)},
		{"entry_unknown", strings.Replace(raw, `"sequence":0`, `"future":{},"sequence":0`, 1)},
		{"entry_duplicate", strings.Replace(raw, `"sequence":0`, `"sequence":0,"sequence":0`, 1)},
		{"entry_case", strings.Replace(raw, `"sequence":0`, `"Sequence":0`, 1)},
		{"entry_missing_sequence", strings.Replace(raw, `"sequence":0,`, ``, 1)},
		{"entry_null_deleted", strings.Replace(raw, `"sequence":0`, `"deleted":null,"sequence":0`, 1)},
		{"entry_string_deleted", strings.Replace(raw, `"sequence":0`, `"deleted":"false","sequence":0`, 1)},
		{"protocol_null", strings.Replace(raw, `"context_protocol":1`, `"context_protocol":null`, 1)},
		{"protocol_missing", strings.Replace(raw, `"context_protocol":1`, ``, 1)},
		{"protocol_unknown", strings.Replace(raw, `"context_protocol":1`, `"future":1,"context_protocol":1`, 1)},
		{"protocol_duplicate", strings.Replace(raw, `"context_protocol":1`, `"context_protocol":0,"context_protocol":1`, 1)},
		{"protocol_case", strings.Replace(raw, `"context_protocol":1`, `"Context_protocol":1`, 1)},
		{"author_null", strings.Replace(raw, `"author":{"name":"Synthetic","email":"","team":""}`, `"author":null`, 1)},
		{"author_duplicate", strings.Replace(raw, `"name":"Synthetic"`, `"name":"Synthetic","name":"Synthetic"`, 1)},
		{"author_unknown", strings.Replace(raw, `"name":"Synthetic"`, `"name":"Synthetic","unknown":1`, 1)},
		{"author_case", strings.Replace(raw, `"name":"Synthetic"`, `"Name":"Synthetic"`, 1)},
		{"author_missing", strings.Replace(raw, `"team":""`, `"other":""`, 1)},
		{"timestamp_null", strings.Replace(raw, `"created_at":"2026-01-02T03:04:05Z"`, `"created_at":null`, 1)},
		{"timestamp_invalid", strings.Replace(raw, `2026-01-02T03:04:05Z`, `yesterday`, 1)},
		{"timestamp_number", strings.Replace(raw, `"created_at":"2026-01-02T03:04:05Z"`, `"created_at":1234`, 1)},
		{"null_branch", strings.Replace(raw, `"branch":"main"`, `"branch":null`, 1)},
		{"string_parents", strings.Replace(raw, `"parents":[]`, `"parents":""`, 1)},
		{"null_parent_element", strings.Replace(raw, `"parents":[]`, `"parents":[null]`, 1)},
		{"number_model", strings.Replace(raw, `"parents":[]`, `"models":[1],"parents":[]`, 1)},
		{"empty_cursor_final", `{"next_cursor":"",` + raw[1:]},
		{"null_cursor", `{"next_cursor":null,` + raw[1:]},
		{"number_cursor", `{"next_cursor":123,` + raw[1:]},
		{"both_cursor_checkpoint", `{"next_cursor":"opaque",` + raw[1:]},
		{"bad_utf8", "\xff" + raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.raw == raw {
				t.Fatal("test mutation did not change the wire")
			}
			if got, err := DecodeCatalogPage([]byte(tc.raw)); !errors.Is(err, ErrHashMismatch) || got.Entries != nil {
				t.Fatalf("DecodeCatalogPage error=%v; returned entries=%d", err, len(got.Entries))
			}
		})
	}
	for _, key := range []string{"version", "repo_id", "epoch", "through", "mode", "entries", "checkpoint"} {
		for _, null := range []bool{false, true} {
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &object); err != nil {
				t.Fatal(err)
			}
			if null {
				object[key] = json.RawMessage(`null`)
			} else {
				delete(object, key)
			}
			if _, err := DecodeCatalogPage(catalogTestRaw(t, object)); err == nil {
				t.Fatalf("accepted required field %s missing/null=%v", key, null)
			}
		}
	}
	for _, key := range []string{"version", "repo_id", "epoch", "sequence"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		var checkpoint map[string]json.RawMessage
		if err := json.Unmarshal(object["checkpoint"], &checkpoint); err != nil {
			t.Fatal(err)
		}
		delete(checkpoint, key)
		object["checkpoint"] = catalogTestRaw(t, checkpoint)
		if _, err := DecodeCatalogPage(catalogTestRaw(t, object)); err == nil {
			t.Fatalf("accepted missing checkpoint field %s", key)
		}
	}
}

func TestCatalogDecodePreservesNullableSnapshotCollections(t *testing.T) {
	snapshot := Snapshot{ID: HashContent([]byte("legacy metadata")), RepoID: catalogTestRepo()}
	snapshot.DocHash = snapshot.ID
	entry := CatalogEntry{Kind: "snapshot", Key: string(snapshot.ID), Value: catalogTestRaw(t, snapshot)}
	entry.Value = append(json.RawMessage(`{"models":null,"graft_parents":null,`), entry.Value[1:]...)
	page, err := DecodeCatalogPage(catalogTestRaw(t, catalogTestPage(entry)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page.Entries[0].Value, entry.Value) {
		t.Fatal("nullable metadata image changed")
	}
	manifest, snapshots, err := CatalogManifest(catalogTestRepo(), append([]CatalogEntry{catalogTestProtocol(t, 1)}, page.Entries...))
	if err != nil || len(snapshots) != 1 || snapshots[0].Parents != nil || snapshots[0].Models != nil || snapshots[0].GraftParents != nil || manifest.Version != 1 {
		t.Fatalf("legacy projection: snapshots=%d err=%v", len(snapshots), err)
	}
}

func TestCatalogDecodePreservesFullHistoryAndRejectsUnknownNestedFields(t *testing.T) {
	repo := catalogTestRepo()
	source := HashContent([]byte("source"))
	gitSHA := strings.Repeat("a", 40)
	event := HistoryEvent{
		ID: strings.Repeat("a", 32), RepoID: repo, Kind: "pr-merge", Branch: "main", BranchID: "main-identity",
		SourceBranchID: "source-identity", Source: source, Target: HashContent([]byte("joined target")), SharedTarget: source,
		PR:          &PullRequestMerge{Number: 4, BaseBranch: "main", HeadBranch: "feature", HeadSHA: gitSHA, MergeSHA: strings.Repeat("b", 40)},
		PRCompleted: true, MemoryHash: HashContent([]byte("memory")), MemorySource: source, MemoryPinned: true,
		BindingParent: strings.Repeat("b", 32), WorktreeID: strings.Repeat("c", 32), GitBefore: gitSHA, GitAfter: strings.Repeat("b", 40),
		CreatedAt: catalogTestSnapshot("snapshot").CreatedAt,
	}
	entry := CatalogEntry{Kind: "history", Key: event.ID, Sequence: 7, Value: catalogTestRaw(t, event)}
	// JSON whitespace and raw metadata survive decoding exactly. No narrow DTO
	// may discard receipt, memory provenance, or worktree/PR fields.
	entry.Value = bytes.ReplaceAll(entry.Value, []byte(`,`), []byte(", \n "))
	prefix := `{"version":1,"repo_id":` + strconv.Quote(repo) + `,"epoch":` + strconv.Quote(catalogTestEpoch) + `,"through":7,"mode":"baseline","entries":[{"sequence":7,"kind":"history","key":` + strconv.Quote(event.ID) + `,"value":`
	suffix := `}],"checkpoint":` + string(catalogTestRaw(t, CatalogCheckpoint{Version: 1, RepoID: repo, Epoch: catalogTestEpoch, Sequence: 7})) + `}`
	raw := prefix + string(entry.Value) + suffix
	page, err := DecodeCatalogPage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var decoded HistoryEvent
	if err := json.Unmarshal(page.Entries[0].Value, &decoded); err != nil || !reflect.DeepEqual(event, decoded) || !bytes.Equal(entry.Value, page.Entries[0].Value) {
		t.Fatal("full history changed")
	}
	for _, replacement := range []string{`"future":true,"number":4`, `"number":4,"number":4`, `"Number":4`, `"number":null`} {
		mutated := strings.Replace(raw, `"number":4`, replacement, 1)
		if _, err := DecodeCatalogPage([]byte(mutated)); err == nil {
			t.Fatalf("accepted malformed PR metadata %s", replacement)
		}
	}
	birth := HistoryEvent{
		ID: strings.Repeat("c", 32), RepoID: repo, Kind: "birth", Branch: "feature", BranchID: "source-identity", GitAfter: gitSHA,
		Creation:  &GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD", StartCommit: gitSHA, OriginBranch: "main", OriginBranchID: "main-identity"},
		CreatedAt: event.CreatedAt,
	}
	birthEntry := CatalogEntry{Kind: "history", Key: birth.ID, Value: catalogTestRaw(t, birth)}
	if err := ValidateCatalogEntry(repo, birthEntry); err != nil {
		t.Fatal(err)
	}
	birthEntry.Value = bytes.Replace(birthEntry.Value, []byte(`"evidence":"process-argv"`), []byte(`"evidence":"process-argv","unknown":true`), 1)
	if ValidateCatalogEntry(repo, birthEntry) == nil {
		t.Fatal("accepted unknown creation evidence field")
	}
}

func TestCatalogWireStructParityWithBackend(t *testing.T) {
	// The modules cannot import each other's internal packages. Parse the
	// server's actual declarations to catch wire/tag drift without a dependency.
	file, err := parser.ParseFile(token.NewFileSet(), "../../../backend/internal/domain/catalog.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours := map[string]reflect.Type{
		"CatalogCheckpoint": reflect.TypeFor[CatalogCheckpoint](),
		"CatalogRequest":    reflect.TypeFor[CatalogRequest](),
		"CatalogEntry":      reflect.TypeFor[CatalogEntry](),
		"CatalogPage":       reflect.TypeFor[CatalogPage](),
	}
	checked := 0
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			cliType, wanted := ours[typeSpec.Name.Name]
			if !wanted {
				continue
			}
			serverType, ok := typeSpec.Type.(*ast.StructType)
			if !ok || len(serverType.Fields.List) != cliType.NumField() {
				t.Fatalf("%s field count changed", typeSpec.Name.Name)
			}
			for i, field := range serverType.Fields.List {
				if field.Tag == nil || len(field.Names) != 1 {
					t.Fatal("unexpected backend wire declaration")
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil || field.Names[0].Name != cliType.Field(i).Name || tag != string(cliType.Field(i).Tag) {
					t.Fatalf("%s field %d/tag drift", typeSpec.Name.Name, i)
				}
				if got := catalogTestASTType(field.Type); got != catalogTestReflectType(cliType.Field(i).Type) {
					t.Fatalf("%s.%s type drift: server=%s cli=%s", typeSpec.Name.Name, field.Names[0].Name, got, catalogTestReflectType(cliType.Field(i).Type))
				}
			}
			checked++
		}
	}
	if checked != len(ours) {
		t.Fatalf("checked %d of %d wire types", checked, len(ours))
	}
}

func catalogTestASTType(expr ast.Expr) string {
	switch typ := expr.(type) {
	case *ast.Ident:
		if typ.Name == "ContentHash" {
			return "string"
		}
		return typ.Name
	case *ast.SelectorExpr:
		return catalogTestASTType(typ.X) + "." + typ.Sel.Name
	case *ast.StarExpr:
		return "*" + catalogTestASTType(typ.X)
	case *ast.ArrayType:
		return "[]" + catalogTestASTType(typ.Elt)
	}
	return "unsupported"
}

func catalogTestReflectType(typ reflect.Type) string {
	if typ.Kind() == reflect.Pointer {
		return "*" + catalogTestReflectType(typ.Elem())
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return "json.RawMessage"
	}
	if typ.Kind() == reflect.Slice {
		return "[]" + catalogTestReflectType(typ.Elem())
	}
	return typ.Name()
}
