package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type docToolSourceCodecFixture struct {
	Tree   DocToolTreeSource
	Node   DocToolTreeNode
	Keys   DocToolKeyMapSource
	Orders DocToolOrdersSource
}

func docToolSourceCodecFixtureFor(test testing.TB, tree DocToolTreeSource, node DocToolTreeNode, nodes int) docToolSourceCodecFixture {
	test.Helper()
	treeRaw, err := canonicalJSON(tree)
	if err != nil {
		test.Fatal(err)
	}
	keys := DocToolKeyMapSource{Version: 1, Profile: DocToolKeyMapProfile, Tree: HashContent(treeRaw), Nodes: nodes}
	keyRaw, err := canonicalJSON(keys)
	if err != nil {
		test.Fatal(err)
	}
	orders := DocToolOrdersSource{Version: 1, Profile: DocToolOrdersProfile, Tree: HashContent(treeRaw), Keys: HashContent(keyRaw), Nodes: nodes}
	return docToolSourceCodecFixture{Tree: tree, Node: node, Keys: keys, Orders: orders}
}

func docToolSourceCodecBase(test testing.TB) docToolSourceCodecFixture {
	test.Helper()
	tree := DocToolTreeSource{Version: 1, Profile: DocToolTreeProfile,
		EventHash: HashContent([]byte("<>&\u2028\u2029\ud55c\U0001f642")), Evidence: HashContent([]byte("evidence")),
		EventBytes: 512, Root: DocFindSpan{From: 16, To: 256}, Type: DocToolValueObject}
	node := DocToolTreeNode{Value: DocFindSpan{From: 64, To: 128}, Type: DocToolValueArray, Parent: 16,
		Ordinal: 1, Key: &DocFindSpan{From: 32, To: 40}, Children: 2, Depth: 1}
	return docToolSourceCodecFixtureFor(test, tree, node, 8)
}

func docToolTreeSourceCodecOracle(source DocToolTreeSource) ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func docToolTreeNodeCodecOracle(tree DocToolTreeSource, node DocToolTreeNode) ([]byte, error) {
	if node.Validate(tree) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(node)
	if err != nil || len(raw) > MaxDocToolTreeNodeBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func docToolKeyMapSourceCodecOracle(tree DocToolTreeSource, source DocToolKeyMapSource) ([]byte, error) {
	treeRaw, err := docToolTreeSourceCodecOracle(tree)
	if err != nil || source.Version != 1 || source.Profile != DocToolKeyMapProfile || source.Tree != HashContent(treeRaw) ||
		source.Nodes <= 0 || source.Nodes > tree.Root.To-tree.Root.From {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func docToolOrdersSourceCodecOracle(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource) ([]byte, error) {
	treeRaw, treeErr := docToolTreeSourceCodecOracle(tree)
	keyRaw, keyErr := docToolKeyMapSourceCodecOracle(tree, keys)
	expected := DocToolOrdersSource{Version: 1, Profile: DocToolOrdersProfile, Tree: HashContent(treeRaw), Keys: HashContent(keyRaw), Nodes: keys.Nodes}
	if treeErr != nil || keyErr != nil || source != expected {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

type docToolSourceCodecCase struct {
	name   string
	value  any
	encode func() ([]byte, error)
	oracle func() ([]byte, error)
	hash   func() (ContentHash, error)
}

func (fixture docToolSourceCodecFixture) cases() []docToolSourceCodecCase {
	return []docToolSourceCodecCase{
		{"tree", fixture.Tree, fixture.Tree.Encode,
			func() ([]byte, error) { return docToolTreeSourceCodecOracle(fixture.Tree) }, fixture.Tree.Hash},
		{"node", fixture.Node, func() ([]byte, error) { return fixture.Node.Encode(fixture.Tree) },
			func() ([]byte, error) { return docToolTreeNodeCodecOracle(fixture.Tree, fixture.Node) }, nil},
		{"keys", fixture.Keys, func() ([]byte, error) { return fixture.Keys.Encode(fixture.Tree) },
			func() ([]byte, error) { return docToolKeyMapSourceCodecOracle(fixture.Tree, fixture.Keys) },
			func() (ContentHash, error) { return fixture.Keys.Hash(fixture.Tree) }},
		{"orders", fixture.Orders, func() ([]byte, error) { return fixture.Orders.Encode(fixture.Tree, fixture.Keys) },
			func() ([]byte, error) {
				return docToolOrdersSourceCodecOracle(fixture.Tree, fixture.Keys, fixture.Orders)
			},
			func() (ContentHash, error) { return fixture.Orders.Hash(fixture.Tree, fixture.Keys) }},
	}
}

func assertDocToolSourceCodecOracle(test testing.TB, fixture docToolSourceCodecFixture, valid bool) {
	test.Helper()
	before, err := json.Marshal(fixture)
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range fixture.cases() {
		want, wantErr := candidate.oracle()
		got, gotErr := candidate.encode()
		if gotErr != wantErr || !bytes.Equal(got, want) || (got == nil) != (want == nil) || valid && gotErr != nil {
			test.Fatalf("%s changed canonical bytes or validation: got=%q (%v), want=%q (%v)", candidate.name, got, gotErr, want, wantErr)
		}
		var expectedHash ContentHash
		if wantErr == nil {
			expectedHash = HashContent(want)
			if HashContent(got) != expectedHash || len(got) > MaxDocDisplaySourceBytes {
				test.Fatal(candidate.name, "changed hash or exceeded the original wire bound")
			}
			decoded := reflect.New(reflect.TypeOf(candidate.value))
			if err := json.Unmarshal(got, decoded.Interface()); err != nil || !reflect.DeepEqual(decoded.Elem().Interface(), candidate.value) {
				test.Fatal(candidate.name, "changed the public JSON shape", err)
			}
			if candidate.name == "tree" {
				decoded, err := DecodeDocToolTreeSource(expectedHash, want)
				if err != nil || decoded != fixture.Tree {
					test.Fatal("original canonical tree no longer decodes", err)
				}
			} else if candidate.name == "node" {
				decoded, err := DecodeDocToolTreeNode(fixture.Tree, expectedHash, want)
				if err != nil || !reflect.DeepEqual(decoded, fixture.Node) {
					test.Fatal("original canonical node no longer decodes", err)
				}
			}
		}
		if candidate.hash != nil {
			hash, err := candidate.hash()
			if hash != expectedHash || err != wantErr {
				test.Fatal(candidate.name, "changed source hash or validation", hash, expectedHash, err, wantErr)
			}
		}
	}
	after, err := json.Marshal(fixture)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("encoding mutated the source or key span", err)
	}
}

func TestDocToolSourceCodecsCanonicalOracle(test *testing.T) {
	assertDocToolSourceCodecOracle(test, docToolSourceCodecBase(test), true)
	for _, scalar := range []struct {
		valueType DocToolValueType
		length    int
	}{{DocToolValueObject, 2}, {DocToolValueArray, 2}, {DocToolValueString, 2}, {DocToolValueNumber, 1},
		{DocToolValueNull, 4}, {DocToolValueTrue, 4}, {DocToolValueFalse, 5}} {
		test.Run(fmt.Sprint(scalar.valueType), func(test *testing.T) {
			for _, from := range []int{0, 17, MaxConversationDocumentBytes - scalar.length} {
				fixture := docToolSourceCodecBase(test)
				fixture.Tree.Root = DocFindSpan{From: from, To: from + scalar.length}
				fixture.Tree.EventBytes = fixture.Tree.Root.To
				fixture.Tree.Type = scalar.valueType
				node := DocToolTreeNode{Value: fixture.Tree.Root, Type: scalar.valueType, Parent: -1}
				assertDocToolSourceCodecOracle(test, docToolSourceCodecFixtureFor(test, fixture.Tree, node, 1), true)
			}
			for _, keyed := range []bool{false, true} {
				fixture := docToolSourceCodecBase(test)
				if !keyed {
					fixture.Tree.Type = DocToolValueArray
					fixture.Node.Key = nil
				}
				fixture.Node.Type, fixture.Node.Children = scalar.valueType, 0
				fixture.Node.Value.To = fixture.Node.Value.From + scalar.length
				assertDocToolSourceCodecOracle(test, docToolSourceCodecFixtureFor(test, fixture.Tree, fixture.Node, 8), true)
			}
		})
	}
	fixture := docToolSourceCodecBase(test)
	fixture.Node.Parent, fixture.Node.Depth = 24, docToolDisplayMaxDepth
	fixture.Node.Children = (fixture.Node.Value.To - fixture.Node.Value.From - 1) / 2
	assertDocToolSourceCodecOracle(test, docToolSourceCodecFixtureFor(test, fixture.Tree, fixture.Node, fixture.Tree.Root.To-fixture.Tree.Root.From), true)
}

func TestDocToolSourceCodecsRejectInvalidBoundsAndText(test *testing.T) {
	for _, mutation := range []struct {
		name   string
		target string
		change func(*docToolSourceCodecFixture)
	}{
		{"tree_version", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.Version = 0 }},
		{"tree_bytes_negative", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.EventBytes = -1 }},
		{"tree_bytes_excess", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.EventBytes = MaxConversationDocumentBytes + 1 }},
		{"tree_from", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.Root.From = -1 }},
		{"tree_to", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.Root.To = -1 }},
		{"tree_type", "tree", func(fixture *docToolSourceCodecFixture) { fixture.Tree.Type = 255 }},
		{"node_from", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Value.From = -1 }},
		{"node_to", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Value.To = -1 }},
		{"node_parent", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Parent = -2 }},
		{"node_ordinal", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Ordinal = -1 }},
		{"node_children", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Children = -1 }},
		{"node_depth_negative", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Depth = -1 }},
		{"node_depth_excess", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Depth = docToolDisplayMaxDepth + 1 }},
		{"node_key_from", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Key.From = -1 }},
		{"node_key_to", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Key.To = -1 }},
		{"node_key_zero", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Key = &DocFindSpan{} }},
		{"node_type", "node", func(fixture *docToolSourceCodecFixture) { fixture.Node.Type = 255 }},
		{"keys_version", "keys", func(fixture *docToolSourceCodecFixture) { fixture.Keys.Version = 0 }},
		{"keys_nodes_negative", "keys", func(fixture *docToolSourceCodecFixture) { fixture.Keys.Nodes = -1 }},
		{"keys_nodes_zero", "keys", func(fixture *docToolSourceCodecFixture) { fixture.Keys.Nodes = 0 }},
		{"keys_nodes_excess", "keys", func(fixture *docToolSourceCodecFixture) { fixture.Keys.Nodes = fixture.Tree.EventBytes + 1 }},
		{"keys_binding", "keys", func(fixture *docToolSourceCodecFixture) { fixture.Keys.Tree = HashContent(nil) }},
		{"orders_version", "orders", func(fixture *docToolSourceCodecFixture) { fixture.Orders.Version = 0 }},
		{"orders_nodes_negative", "orders", func(fixture *docToolSourceCodecFixture) { fixture.Orders.Nodes = -1 }},
		{"orders_nodes_mismatch", "orders", func(fixture *docToolSourceCodecFixture) { fixture.Orders.Nodes++ }},
		{"orders_tree_binding", "orders", func(fixture *docToolSourceCodecFixture) { fixture.Orders.Tree = HashContent(nil) }},
		{"orders_key_binding", "orders", func(fixture *docToolSourceCodecFixture) { fixture.Orders.Keys = HashContent(nil) }},
	} {
		test.Run(mutation.name, func(test *testing.T) {
			fixture := docToolSourceCodecBase(test)
			mutation.change(&fixture)
			assertDocToolSourceCodecOracle(test, fixture, false)
			for _, candidate := range fixture.cases() {
				if candidate.name == mutation.target {
					if raw, err := candidate.encode(); err != ErrIntegrity || raw != nil {
						test.Fatal("invalid bounds or binding returned bytes", raw, err)
					}
				}
			}
		})
	}
	for _, text := range []string{"", "<>&\"\\/\n\t\u0000", "\u2028\u2029\ud55c\U0001f642\ufffd", "\xff\xed\xa0\x80"} {
		fixture := docToolSourceCodecBase(test)
		for _, source := range []any{&fixture.Tree, &fixture.Keys, &fixture.Orders} {
			original := reflect.ValueOf(source).Elem()
			for index := 0; index < original.NumField(); index++ {
				if original.Field(index).Kind() != reflect.String {
					continue
				}
				prior := original.Field(index).String()
				original.Field(index).SetString(text)
				assertDocToolSourceCodecOracle(test, fixture, false)
				original.Field(index).SetString(prior)
			}
		}
	}
}

func FuzzDocToolSourceCodecsCanonicalOracle(fuzzer *testing.F) {
	fixture := docToolSourceCodecBase(fuzzer)
	raw, err := json.Marshal(fixture)
	if err != nil {
		fuzzer.Fatal(err)
	}
	fuzzer.Add(raw)
	for _, raw := range []string{`{}`, `{"Node":{"Key":null}}`, `{"Node":{"Key":{"From":0,"To":0}}}`,
		`{"Tree":{"Root":{"From":-1},"Type":255}}`, `{"Tree":{"Profile":"<>&\u2028\ud55c"}}`,
		`{"Node":{"Parent":-9223372036854775808,"Ordinal":9223372036854775807}}`,
		`{"Keys":{"Nodes":-1},"Orders":{"Nodes":0}}`} {
		fuzzer.Add([]byte(raw))
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 8192 {
			return
		}
		fixture := docToolSourceCodecBase(test)
		if json.Unmarshal(raw, &fixture) != nil {
			return
		}
		assertDocToolSourceCodecOracle(test, fixture, false)
	})
}

func BenchmarkDocToolSourceCodecs(benchmark *testing.B) {
	fixture := docToolSourceCodecBase(benchmark)
	for _, candidate := range fixture.cases() {
		want, err := candidate.oracle()
		if err != nil {
			benchmark.Fatal(err)
		}
		for _, implementation := range []struct {
			name   string
			encode func() ([]byte, error)
		}{{"oracle", candidate.oracle}, {"typed", candidate.encode}} {
			benchmark.Run(candidate.name+"/"+implementation.name, func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				benchmark.SetBytes(int64(len(want)))
				for benchmark.Loop() {
					got, err := implementation.encode()
					if err != nil || !bytes.Equal(got, want) {
						benchmark.Fatal("encoder changed canonical bytes", err)
					}
				}
			})
		}
	}
}
