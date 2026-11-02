package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func graphProjectionTestJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func graphProjectionTestState(repo ContentHash) GraphState {
	first, second := HashContent([]byte("first")), HashContent([]byte("second"))
	stamp := time.Unix(1234567890, 123456789).UTC()
	return GraphState{
		Version: 1, Revision: RepositoryRevision{Graph: 1<<63 + 19, Pending: 37, Evidence: 1<<64 - 1}, PrimaryBranch: "main",
		Integrations: []GraphIntegration{
			{Branch: "topic", Scope: "topic-id", Target: second, Head: "merge", Parents: map[string][]string{"merge": {string(second), string(first)}, "empty": {}, "nil": nil}, ExtraParents: []string{"second", "first"}},
			{Branch: "main", Parents: map[string][]string{}, ExtraParents: nil},
			{Parents: nil, ExtraParents: []string{}},
		},
		BranchContexts: map[string]BranchContext{
			"main": {BranchID: "main-id", SnapshotID: second, CodeCommit: strings.Repeat("a", 40), Reason: "kept <verbatim> & \ud55c\uae00", Merges: []BranchContextMerge{
				{DeliveryAcknowledged: true, DestinationBranchID: "main-id", EventID: "merge-event", Source: first, Before: second, MergeSHA: strings.Repeat("b", 40), PRNumber: 23, State: "included", Reason: "verified", Order: 2},
				{EventID: "review", State: "review", Order: -1},
			}, Roots: []ContentHash{first, second}, SnapshotIDs: []ContentHash{second, first}},
			"empty": {Merges: []BranchContextMerge{}, Roots: nil, SnapshotIDs: []ContentHash{}},
			"nil":   {},
		},
		BranchSnapshots: map[string][]ContentHash{"main": {second, first}, "empty": {}, "nil": nil},
		Continuations:   map[ContentHash]string{second: "session-z", first: "session-a"},
		Operations: GraphOperations{
			Births: []GraphBirth{{ID: "birth-z", EventID: "event-z", Branch: "topic", Source: first, Orphan: true, CreatedAt: stamp, Children: []ContentHash{second, first}}, {ID: "empty", Children: []ContentHash{}}, {ID: "nil"}},
			Merges: []GraphMerge{
				{ID: "merge", EventID: "merge-event", Before: first, After: second, Source: first, SourceArms: []ContentHash{second, first}, Branch: "main", Scope: "main-id", From: "topic", CreatedAt: stamp, PRNumber: 23, HistoricalOnly: true, Withdrawn: true, RepresentedGrafts: []ContentHash{second}, RedirectChildren: []ContentHash{}, SourceBirth: "source-birth", LifecycleBirth: "lifecycle-birth", identity: "private-source-identity"},
				{ID: "empty", SourceArms: []ContentHash{}, RepresentedGrafts: []ContentHash{}, RedirectChildren: nil},
				{ID: "nil", SourceArms: nil, RedirectChildren: []ContentHash{}},
			},
		},
		SnapshotIDs: []ContentHash{second, first, second}, GraphIDs: []ContentHash{second, first}, CommittedIDs: []ContentHash{}, HistoricalIDs: nil,
		SharedIDs: []ContentHash{first}, PushedIDs: []ContentHash{first}, UnpushedIDs: []ContentHash{second}, UncommittedIDs: []ContentHash{},
		TaggedIDs: nil, ArchivedOnlyIDs: []ContentHash{second}, AheadIDs: []ContentHash{second, first}, AheadTips: []ContentHash{second},
		Markers:     []GraphBranchMarker{{Branch: "topic", Target: second, Kind: "joined", UniqueCount: 5, TargetAvailable: true}, {Branch: "other", Kind: "archived"}},
		BranchHeads: map[string]GraphBranchHead{"topic-id": {Branch: "topic", EventID: "archive", Archived: true}, "main-id": {Branch: "main", EventID: "position"}},
		RefScopes:   map[string]string{"main": "main-id"}, SnapshotScopes: map[string]string{}, ScopeLabels: map[string]string{"topic-id": "topic", "main-id": "main"},
		Hold: []GraphHoldCluster{
			{Tips: []Unsync{{RepoID: repo, User: "peer", Branch: "topic", Target: second, Author: TeamIdentity{Name: "name", Email: "name@example.test", Team: "team"}, UpdatedAt: stamp}}, IDs: []ContentHash{second, first}},
			{Tips: nil, IDs: []ContentHash{}}, {Tips: []Unsync{}, IDs: nil},
		},
		OrphanSessions: []string{"session-z", "session-a", "session-z"}, HoldCounts: map[string]int{"topic": 3, "main": 0},
		Positions: []GraphPosition{{EventID: "position-z", Branch: "topic", BranchID: "topic-id", Snapshot: second, Archived: true, CreatedAt: stamp}, {EventID: "position-a", CreatedAt: stamp.Add(-time.Hour)}},
		Previous:  []GraphProgressGroup{{Key: "previous", Branch: "topic", Before: second, After: first, CreatedAt: stamp, SnapshotIDs: []ContentHash{second, first}, CollapsibleIDs: []ContentHash{}}, {SnapshotIDs: nil, CollapsibleIDs: nil}},
	}
}

func graphProjectionTestParts(t *testing.T, graph GraphState) (ContentHash, []GraphProjectionPart) {
	t.Helper()
	repo := HashContent([]byte("graph projection repository"))
	parts, err := EncodeGraphProjection(context.Background(), repo, graph)
	if err != nil {
		t.Fatal(err)
	}
	return repo, parts
}

func graphProjectionTestIndex(t *testing.T, parts []GraphProjectionPart, key string) int {
	t.Helper()
	for index, part := range parts {
		if part.Key == key {
			return index
		}
	}
	t.Fatalf("missing test part %s", key)
	return -1
}

func graphProjectionTestRebind(t *testing.T, parts []GraphProjectionPart, key string, raw []byte) {
	t.Helper()
	parts[graphProjectionTestIndex(t, parts, key)].Value = raw
	headerIndex := graphProjectionTestIndex(t, parts, "view")
	var header graphProjectionHeader
	if err := json.Unmarshal(parts[headerIndex].Value, &header); err != nil {
		t.Fatal(err)
	}
	for index := range header.Parts {
		if header.Parts[index].Key == key {
			header.Parts[index].Digest = HashContent(raw)
		}
	}
	parts[headerIndex].Value = graphProjectionTestJSON(t, header)
}

func TestGraphProjectionSchemaDrift(t *testing.T) {
	t.Run("GraphState", func(t *testing.T) {
		var graph GraphState
		var operations graphProjectionOperations
		graphValue := reflect.ValueOf(&graph).Elem()
		expected := map[string]reflect.StructField{}
		for index := 0; index < graphValue.NumField(); index++ {
			field := graphValue.Type().Field(index)
			if field.Name == "ReadGeneration" {
				if field.Type != reflect.TypeFor[*GraphReadGeneration]() || field.Tag.Get("json") != "read_generation,omitempty" {
					t.Fatal("runtime read generation contract changed")
				}
				continue
			}
			key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if !field.IsExported() || key == "-" {
				continue
			}
			if key == "" {
				key = field.Name
			}
			if _, exists := expected[key]; exists || field.Anonymous {
				t.Fatalf("ambiguous GraphState JSON field %s", field.Name)
			}
			expected[key] = field
		}
		for _, field := range graphProjectionFields(&graph, &operations) {
			definition, exists := expected[field.key]
			if !exists {
				t.Errorf("unknown or duplicate projection field %q", field.key)
				continue
			}
			delete(expected, field.key)
			want := graphValue.FieldByIndex(definition.Index).Addr()
			if definition.Name == "Operations" {
				if definition.Type != reflect.TypeFor[GraphOperations]() {
					t.Fatalf("GraphState.Operations changed type to %v", definition.Type)
				}
				want = reflect.ValueOf(&operations)
			}
			value := reflect.ValueOf(field.value).FieldByName("Value")
			if !value.IsValid() || value.Type() != want.Type() {
				t.Errorf("projection field %q must reference %s with type %v", field.key, definition.Name, want.Type())
				continue
			}
			if value.Pointer() != want.Pointer() || field.decode == nil {
				t.Errorf("projection field %q is not bound to %s", field.key, definition.Name)
			}
		}
		for key, field := range expected {
			t.Errorf("GraphState.%s (%s, %v) is absent from the projection schema", field.Name, key, field.Type)
		}
	})
	for _, schema := range []struct {
		name   string
		domain reflect.Type
		wire   reflect.Type
	}{
		{"GraphOperations", reflect.TypeFor[GraphOperations](), reflect.TypeFor[graphProjectionOperations]()},
		{"GraphMerge", reflect.TypeFor[GraphMerge](), reflect.TypeFor[graphProjectionMerge]()},
	} {
		t.Run(schema.name, func(t *testing.T) {
			if schema.domain.NumField() != schema.wire.NumField() {
				t.Errorf("%s has %d fields, projection mirror has %d", schema.name, schema.domain.NumField(), schema.wire.NumField())
			}
			for index := 0; index < schema.domain.NumField(); index++ {
				field := schema.domain.Field(index)
				name, fieldType, tag := field.Name, field.Type, field.Tag.Get("json")
				if schema.name == "GraphOperations" && name == "Merges" {
					if fieldType != reflect.TypeFor[[]GraphMerge]() {
						t.Fatalf("GraphOperations.Merges changed type to %v", fieldType)
					}
					fieldType = reflect.TypeFor[[]graphProjectionMerge]()
				}
				if schema.name == "GraphMerge" && name == "identity" {
					name, tag = "Identity", "identity"
				}
				if schema.name == "GraphMerge" && name == "SourceArms" {
					tag = strings.Replace(tag, ",omitempty", "", 1)
				}
				mirror, exists := schema.wire.FieldByName(name)
				if !exists || !mirror.IsExported() || mirror.Anonymous || mirror.Type != fieldType || mirror.Tag.Get("json") != tag {
					t.Errorf("%s.%s must be represented as exported %s %v with JSON tag %q; got %+v", schema.name, field.Name, name, fieldType, tag, mirror)
				}
			}
		})
	}
}

func TestGraphProjectionRichRoundTrip(t *testing.T) {
	repo := HashContent([]byte("graph projection repository"))
	want := graphProjectionTestState(repo)
	before := graphProjectionTestJSON(t, want)
	_, parts := graphProjectionTestParts(t, want)
	fields := graphProjectionFields(&GraphState{}, &graphProjectionOperations{})
	if len(parts) != len(fields)+1 {
		t.Fatal("graph is not split into all explicit top-level fields")
	}
	canonical := graphProjectionTestJSON(t, parts)
	random := rand.New(rand.NewSource(31))
	for iteration := 0; iteration < 8; iteration++ {
		random.Shuffle(len(parts), func(left, right int) { parts[left], parts[right] = parts[right], parts[left] })
		partsBefore := graphProjectionTestJSON(t, parts)
		got, err := DecodeGraphProjection(context.Background(), repo, parts)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("lossy round trip: %v\ngot %#v\nwant %#v", err, got, want)
		}
		next, err := EncodeGraphProjection(context.Background(), repo, got)
		if err != nil || !bytes.Equal(canonical, graphProjectionTestJSON(t, next)) {
			t.Fatal("unstable encoding", err)
		}
		got.BranchContexts["main"].Merges[0].Reason = "changed"
		got.Integrations[0].Parents["merge"][0] = "changed"
		got.Operations.Merges[0].SourceArms[0] = "changed"
		got.Hold[0].IDs[0] = "changed"
		if !bytes.Equal(partsBefore, graphProjectionTestJSON(t, parts)) || !bytes.Equal(before, graphProjectionTestJSON(t, want)) {
			t.Fatal("codec mutated or retained caller data")
		}
	}
	header := parts[graphProjectionTestIndex(t, parts, "view")]
	kinds, keys, err := GraphProjectionPartKeys(header)
	if err != nil || len(keys) != len(fields) || len(kinds) != len(keys) {
		t.Fatal("invalid body discovery", err)
	}
	for index, key := range keys {
		part := parts[graphProjectionTestIndex(t, parts, key)]
		if kinds[index] != part.Kind || part.Kind != "field" {
			t.Fatal("body identity mismatch")
		}
	}
}

func TestGraphProjectionDirectProjectionRoundTrip(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	root, source, head := graphFixture("root"), graphFixture("source"), graphFixture("head")
	root.RepoID, source.RepoID, head.RepoID = repo, repo, repo
	root.CreatedAt, source.CreatedAt, head.CreatedAt = root.CreatedAt.UTC(), source.CreatedAt.UTC(), head.CreatedAt.UTC()
	source.Parents, head.Parents, head.GraftParents = []ContentHash{root.ID}, []ContentHash{root.ID}, []ContentHash{source.ID}
	source.Branch = "topic"
	view := RepositoryView{
		Revision: RepositoryRevision{Graph: 7, Pending: 11, Evidence: 13}, Snapshots: []Snapshot{head, source, root},
		Refs: []Ref{{RepoID: repo, Kind: RefBranch, Name: "main", BranchID: "main-id", Target: head.ID}, {RepoID: repo, Kind: RefBranch, Name: "topic", BranchID: "topic-id", Target: source.ID}},
		History: []HistoryEvent{
			{ID: "birth", Kind: "birth", Branch: "topic", BranchID: "topic-id", Source: root.ID, Target: root.ID, CreatedAt: root.CreatedAt},
			{ID: "merge", Kind: "pr-merge", Branch: "main", BranchID: "main-id", Source: source.ID, Target: head.ID, SharedTarget: root.ID, SourceBranchID: "topic-id", PR: &PullRequestMerge{Number: 23, BaseBranch: "main", HeadBranch: "topic"}, CreatedAt: head.CreatedAt},
		},
		Semantics: ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{{EventID: "merge", Completed: true, PlacementIntact: true, SourceAvailable: true}}},
	}
	want, err := ProjectGraphState(view, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	want.BranchContexts["main"] = BranchContext{BranchID: "main-id", SnapshotID: head.ID, SnapshotIDs: []ContentHash{head.ID, source.ID, root.ID}, Roots: []ContentHash{source.ID, head.ID}, Merges: []BranchContextMerge{{DestinationBranchID: "main-id", EventID: "merge", Source: source.ID, Before: root.ID, State: "included"}}}
	ApplyGraphIntegrations(&want, view.Snapshots)
	if len(want.Operations.Merges) == 0 || want.Operations.Merges[0].identity == "" || len(want.Integrations) == 0 {
		t.Fatal("direct fixture lacks merge identity or integrations")
	}
	parts, err := EncodeGraphProjection(context.Background(), repo, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeGraphProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("direct projection parity: %v\ngot %#v\nwant %#v", err, got, want)
	}
}

func TestGraphProjectionNilEmptyAndStableFields(t *testing.T) {
	for _, nonnil := range []bool{false, true} {
		graph := GraphState{Version: 1}
		if nonnil {
			value := reflect.ValueOf(&graph).Elem()
			for index := 0; index < value.NumField(); index++ {
				field := value.Field(index)
				switch field.Kind() {
				case reflect.Map:
					field.Set(reflect.MakeMap(field.Type()))
				case reflect.Slice:
					field.Set(reflect.MakeSlice(field.Type(), 0, 0))
				}
			}
			graph.Operations = GraphOperations{Births: []GraphBirth{}, Merges: []GraphMerge{}}
		}
		repo, parts := graphProjectionTestParts(t, graph)
		got, err := DecodeGraphProjection(context.Background(), repo, parts)
		if err != nil || !reflect.DeepEqual(got, graph) {
			t.Fatal("nil/empty graph lost", nonnil, err)
		}
		graph.Revision.Graph++
		next, err := EncodeGraphProjection(context.Background(), repo, graph)
		if err != nil {
			t.Fatal(err)
		}
		for index, part := range next {
			if part.Key != "view" && part.Key != "revision" && !reflect.DeepEqual(part, parts[index]) {
				t.Fatalf("unchanged field moved under a generation key: %s", part.Key)
			}
		}
	}
}

func TestGraphProjectionJSONStorageNormalization(t *testing.T) {
	repo := HashContent([]byte("graph projection repository"))
	want := graphProjectionTestState(repo)
	_, parts := graphProjectionTestParts(t, want)
	for index, part := range parts {
		decoder := json.NewDecoder(bytes.NewReader(part.Value))
		decoder.UseNumber()
		var normalized any
		if err := decoder.Decode(&normalized); err != nil {
			t.Fatal(err)
		}
		var raw bytes.Buffer
		encoder := json.NewEncoder(&raw)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", " ")
		if err := encoder.Encode(normalized); err != nil {
			t.Fatal(err)
		}
		parts[index].Value = raw.Bytes()
	}
	got, err := DecodeGraphProjection(context.Background(), repo, parts)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("JSON storage normalization broke digests", err)
	}
}

func TestGraphProjectionCorruptParts(t *testing.T) {
	repo, original := graphProjectionTestParts(t, GraphState{Version: 1, SnapshotIDs: []ContentHash{HashContent([]byte("snapshot"))}})
	cases := []struct {
		name   string
		change func([]GraphProjectionPart) []GraphProjectionPart
	}{
		{"missing header", func(parts []GraphProjectionPart) []GraphProjectionPart { return parts[:len(parts)-1] }},
		{"missing body", func(parts []GraphProjectionPart) []GraphProjectionPart { return parts[1:] }},
		{"extra body", func(parts []GraphProjectionPart) []GraphProjectionPart {
			return append(parts, GraphProjectionPart{Kind: "field", Key: "extra", Value: []byte(`{"value":null}`)})
		}},
		{"duplicate body", func(parts []GraphProjectionPart) []GraphProjectionPart { return append(parts, parts[0]) }},
		{"duplicate header", func(parts []GraphProjectionPart) []GraphProjectionPart { return append(parts, parts[len(parts)-1]) }},
		{"hash mismatch", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[graphProjectionTestIndex(t, parts, "snapshot_ids")].Value = []byte(`{"value":[]}`)
			return parts
		}},
		{"wrong body identity", func(parts []GraphProjectionPart) []GraphProjectionPart { parts[0].Key = "unknown"; return parts }},
		{"wrong body kind", func(parts []GraphProjectionPart) []GraphProjectionPart { parts[0].Kind = "snapshot"; return parts }},
		{"wrong header identity", func(parts []GraphProjectionPart) []GraphProjectionPart {
			parts[len(parts)-1].Key = "graph"
			return parts
		}},
		{"swapped fields", func(parts []GraphProjectionPart) []GraphProjectionPart {
			left, right := graphProjectionTestIndex(t, parts, "snapshot_ids"), graphProjectionTestIndex(t, parts, "graph_ids")
			parts[left].Value, parts[right].Value = parts[right].Value, parts[left].Value
			return parts
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts := test.change(append([]GraphProjectionPart(nil), original...))
			got, err := DecodeGraphProjection(context.Background(), repo, parts)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(got, GraphState{}) {
				t.Fatal("corruption returned data", err)
			}
		})
	}
}

func TestGraphProjectionStrictNestedSchema(t *testing.T) {
	repo := HashContent([]byte("graph projection repository"))
	_, original := graphProjectionTestParts(t, graphProjectionTestState(repo))
	cases := []struct {
		name string
		key  string
		from string
		to   string
	}{
		{"unknown context merge", "branch_contexts", `"state":"included"`, `"state":"included","unknown":true`},
		{"duplicate context merge", "branch_contexts", `"state":"included"`, `"state":"included","state":"review"`},
		{"case alias", "branch_contexts", `"state":"included"`, `"state":"included","STATE":"review"`},
		{"missing required nested field", "branch_contexts", `"order":2`, `"optional_unknown":2`},
		{"null nested object", "branch_contexts", `"nil":{"branch_id":"","snapshot_id":"","reason":"","merges":null,"roots":null,"snapshot_ids":null}`, `"nil":null`},
		{"duplicate map key", "integrations", `"empty":[]`, `"empty":[],"empty":null`},
		{"unknown operation", "operations", `"identity":"private-source-identity"`, `"identity":"private-source-identity","unknown":true`},
		{"null optional primitive", "operations", `"pr_number":23`, `"pr_number":null`},
		{"unknown author", "hold", `"team":"team"`, `"team":"team","unknown":true`},
		{"duplicate author", "hold", `"team":"team"`, `"team":"team","team":"other"`},
		{"invalid timestamp", "positions", `"2009-02-13T23:31:30.123456789Z"`, `"not-a-time"`},
		{"missing revision field", "revision", `,"pending":"37"`, ``},
		{"wrong revision number type", "revision", `"pending":"37"`, `"pending":37`},
		{"wrong value casing", "version", `"value"`, `"VALUE"`},
		{"null scalar", "version", `{"value":1}`, `{"value":null}`},
		{"missing scalar", "version", `{"value":1}`, `{}`},
		{"duplicate scalar", "version", `{"value":1}`, `{"value":1,"value":1}`},
		{"unknown scalar field", "version", `{"value":1}`, `{"value":1,"unknown":0}`},
		{"graph version", "version", `{"value":1}`, `{"value":2}`},
		{"position-specific graph", "position_event", `{"value":""}`, `{"value":"position-z"}`},
		{"null row", "version", `{"value":1}`, `null`},
		{"array row", "version", `{"value":1}`, `[1]`},
		{"trailing JSON", "version", `{"value":1}`, `{"value":1} {}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parts := append([]GraphProjectionPart(nil), original...)
			raw := parts[graphProjectionTestIndex(t, parts, test.key)].Value
			if !bytes.Contains(raw, []byte(test.from)) {
				t.Fatalf("fixture is missing %s", test.from)
			}
			raw = bytes.Replace(raw, []byte(test.from), []byte(test.to), 1)
			graphProjectionTestRebind(t, parts, test.key, raw)
			if _, err := DecodeGraphProjection(context.Background(), repo, parts); !errors.Is(err, ErrIntegrity) {
				t.Fatal("accepted malformed typed JSON", err)
			}
		})
	}
}

func TestGraphProjectionHeaderSchemaBeforeDiscovery(t *testing.T) {
	repo, parts := graphProjectionTestParts(t, GraphState{Version: 1})
	header := parts[graphProjectionTestIndex(t, parts, "view")]
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"version", `"version":1`, `"version":2`},
		{"rules", `"rules":1`, `"rules":2`},
		{"projection", `"projection":"graph-v1"`, `"projection":"on-hold-v1"`},
		{"repo", string(repo), "not-a-repo"},
		{"missing rules", `"rules":1,`, ``},
		{"null rules", `"rules":1`, `"rules":null`},
		{"unknown field", `"rules":1`, `"rules":1,"unknown":true`},
		{"duplicate field", `"rules":1`, `"rules":1,"rules":1`},
		{"case alias", `"rules":1`, `"Rules":1`},
		{"unknown nested field", `"kind":"field"`, `"kind":"field","unknown":0`},
		{"duplicate nested field", `"kind":"field"`, `"kind":"field","kind":"field"`},
		{"invalid body kind", `"kind":"field"`, `"kind":"header"`},
		{"unknown key", `"key":"integrations"`, `"key":"unknown"`},
		{"duplicate key", `"key":"integrations"`, `"key":"branch_contexts"`},
		{"missing digest", `"digest":"sha256:`, `"unexpected_digest":"sha256:`},
		{"invalid digest", `"digest":"sha256:`, `"digest":"sha512:`},
		{"trailing JSON", `"version":1`, `"version":1} {"version":1`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			changed := header
			changed.Value = bytes.Replace(header.Value, []byte(test.from), []byte(test.to), 1)
			kinds, keys, err := GraphProjectionPartKeys(changed)
			if !errors.Is(err, ErrIntegrity) || kinds != nil || keys != nil {
				t.Fatal("invalid header exposed storage keys", err)
			}
			altered := append([]GraphProjectionPart(nil), parts...)
			altered[len(altered)-1] = changed
			if _, err := DecodeGraphProjection(context.Background(), repo, altered); !errors.Is(err, ErrIntegrity) {
				t.Fatal("decode accepted invalid header", err)
			}
		})
	}
}

func TestGraphProjectionBindingsAndLease(t *testing.T) {
	repo, parts := graphProjectionTestParts(t, GraphState{Version: 1})
	foreign := HashContent([]byte("foreign"))
	for _, invalid := range []ContentHash{"", "invalid", foreign} {
		if _, err := DecodeGraphProjection(context.Background(), invalid, parts); !errors.Is(err, ErrIntegrity) {
			t.Fatal("decoded against wrong repository", err)
		}
	}
	for _, graph := range []GraphState{{}, {Version: 2}, {Version: 1, PositionEvent: "position"}} {
		if _, err := EncodeGraphProjection(context.Background(), repo, graph); !errors.Is(err, ErrIntegrity) {
			t.Fatal("encoded unsupported graph", err)
		}
	}
	if _, err := EncodeGraphProjection(context.Background(), "invalid", GraphState{Version: 1}); !errors.Is(err, ErrIntegrity) {
		t.Fatal("encoded invalid repository", err)
	}
	lease := GraphProjectionLease{RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", AppliedSequence: 7}
	request := lease.SourceRequest()
	if request.After == nil || request.After.Scope != RepositoryGraphSourceScope || request.After.RepoID != repo || request.After.Sequence != 7 || request.After.Epoch != lease.Epoch || request.Validate() != nil {
		t.Fatalf("wrong graph source request: %+v", request)
	}
	if PendingProjectionLease(lease).SourceRequest().After.Scope != RepositoryViewSourceScope {
		t.Fatal("graph lease changed pending scope")
	}
	lease.Cursor = "page-cursor"
	if request := lease.SourceRequest(); request.After != nil || request.Cursor != lease.Cursor || request.Version != RepositoryViewSourceVersion {
		t.Fatal("cursor continuation lost")
	}
	lease.Cursor, lease.AppliedSequence = "", -1
	if lease.SourceRequest().After != nil {
		t.Fatal("initial baseline resumed a checkpoint")
	}
	if reflect.TypeFor[GraphProjectionLease]() == reflect.TypeFor[PendingProjectionLease]() || reflect.TypeFor[GraphProjectionRead]() == reflect.TypeFor[PendingProjectionRead]() || reflect.TypeFor[GraphProjectionPart]() != reflect.TypeFor[PendingProjectionPart]() {
		t.Fatal("graph boundary type aliases are wrong")
	}
	read := GraphProjectionRead{Ready: true, Enabled: true, NeedsAttention: true, Dataset: "dataset", Source: *request.After, Applied: *request.After, Parts: parts}
	if !read.Ready || !read.Enabled || !read.NeedsAttention || read.Dataset != "dataset" || read.Source != read.Applied || !reflect.DeepEqual(read.Parts, parts) || ErrGraphProjectionNotReady == ErrPendingProjectionNotReady || ErrGraphProjectionFailed == ErrPendingProjectionFailed {
		t.Fatal("graph read fields or error boundaries lost")
	}
}
