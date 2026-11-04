package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

type docToolKeyMapFixture struct {
	tree        *docToolTreeFixture
	source      DocToolKeyMapSource
	state       DocToolKeyMapState
	states      []DocToolKeyMapState
	steps       []DocToolKeyMapStep
	items       []DocToolKeyItem
	rawBytes    int
	objectReads int
}

func newDocToolKeyMapFixture(test testing.TB, value string) *docToolKeyMapFixture {
	return newDocToolKeyMapFixtureRaw(test, `{"kind":"tool_result","output":`+value+`}`)
}

func newDocToolKeyMapFixtureRaw(test testing.TB, raw string) *docToolKeyMapFixture {
	test.Helper()
	tree := newDocToolTreeFixture(test, raw)
	if err := tree.scan(test); err != nil {
		test.Fatal(err)
	}
	complete := tree.steps[len(tree.steps)-1].State()
	sort.Slice(tree.nodes, func(left, right int) bool { return tree.nodes[left].Value.From < tree.nodes[right].Value.From })
	source, err := NewDocToolKeyMapSource(tree.source, complete)
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocToolKeyMapState(tree.source, source)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := state.Encode(tree.source, source)
	if err != nil {
		test.Fatal(err)
	}
	state, err = DecodeDocToolKeyMapState(tree.source, source, HashContent(encoded), encoded)
	if err != nil {
		test.Fatal("initial checkpoint round trip", err)
	}
	return &docToolKeyMapFixture{tree: tree, source: source, state: state, states: []DocToolKeyMapState{state}}
}

func (fixture *docToolKeyMapFixture) loadNodes(_ context.Context, after, limit int) ([]DocToolTreeNode, error) {
	start := sort.Search(len(fixture.tree.nodes), func(index int) bool { return fixture.tree.nodes[index].Value.From > after })
	return append([]DocToolTreeNode(nil), fixture.tree.nodes[start:min(start+limit, len(fixture.tree.nodes))]...), nil
}

func (fixture *docToolKeyMapFixture) advance(test testing.TB) {
	test.Helper()
	prior, err := fixture.state.Encode(fixture.tree.source, fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	reads, rawBytes, nodeReads := 0, 0, 0
	step, err := StepDocToolKeyMap(context.Background(), fixture.tree.source, fixture.source, fixture.state,
		func(ctx context.Context, from, length int) ([]byte, error) {
			insideKey := false
			for _, node := range fixture.tree.nodes {
				if node.Key != nil && from >= node.Key.From && from+length <= node.Key.To {
					insideKey = true
					break
				}
			}
			if !insideKey || length <= 0 || length > 64<<10 {
				test.Fatal("read escaped key token or exceeded raw budget", from, length)
			}
			rawBytes += length
			return fixture.tree.read(ctx, from, length)
		},
		func(ctx context.Context, hash ContentHash) ([]byte, error) {
			reads++
			return fixture.tree.load(ctx, hash)
		},
		func(ctx context.Context, after, limit int) ([]DocToolTreeNode, error) {
			nodeReads++
			expected := min(16, fixture.source.Nodes-fixture.state.Seen)
			if fixture.state.Current != nil {
				expected = 1
			}
			if after != fixture.state.After || limit != expected {
				test.Fatal("wrong keyset page request", after, limit, fixture.state, expected)
			}
			return fixture.loadNodes(ctx, after, limit)
		})
	if err != nil {
		test.Fatal("key-map step", fixture.state.Phase, err)
	}
	unchanged, err := fixture.state.Encode(fixture.tree.source, fixture.source)
	if err != nil || !bytes.Equal(prior, unchanged) || step.Prior() != HashContent(prior) || step.Source() != fixture.source {
		test.Fatal("step mutated predecessor or lost source binding", err)
	}
	if nodeReads != 1 || len(step.Inputs()) == 0 || len(step.Inputs()) > 16 || len(step.Items()) > len(step.Inputs()) || rawBytes != step.ReadBytes() || rawBytes > 64<<10 {
		test.Fatal("step exceeded input/raw contract", nodeReads, len(step.Inputs()), len(step.Items()), rawBytes, step.ReadBytes())
	}
	inputs := step.Inputs()
	for index, node := range inputs {
		if node.Validate(fixture.tree.source) != nil || index > 0 && inputs[index-1].Value.From >= node.Value.From {
			test.Fatal("invalid or unordered consumed node")
		}
	}
	for _, item := range step.Items() {
		if item.Validate(fixture.tree.source) != nil || !slices.ContainsFunc(inputs, func(node DocToolTreeNode) bool { return reflect.DeepEqual(node, item.Node) }) {
			test.Fatal("item was not a validated consumed node")
		}
	}
	for _, object := range step.Objects() {
		if object.Hash != HashContent(object.Bytes) {
			test.Fatal("unverifiable emitted object")
		}
		if existing, found := fixture.tree.objects[object.Hash]; found && !bytes.Equal(existing, object.Bytes) {
			test.Fatal("content-addressed object was overwritten")
		}
		fixture.tree.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	next, err := step.State().Encode(fixture.tree.source, fixture.source)
	if err != nil || bytes.Equal(prior, next) || len(next) > 64<<10 || bytes.Contains(next, []byte("PUBLIC_BODY_MARKER")) || bytes.Contains(next, []byte("PRIVATE_BODY_MARKER")) {
		test.Fatal("nonprogressing, unbounded, or body-bearing checkpoint", len(next), err)
	}
	state, err := DecodeDocToolKeyMapState(fixture.tree.source, fixture.source, HashContent(next), next)
	if err != nil || !reflect.DeepEqual(state, step.State()) {
		test.Fatal("checkpoint did not survive serialized restart", err)
	}
	if state.Active != "" {
		checkpoint := fixture.tree.objects[state.Active]
		if bytes.Contains(checkpoint, []byte("PUBLIC_BODY_MARKER")) || bytes.Contains(checkpoint, []byte("PRIVATE_BODY_MARKER")) {
			test.Fatal("nested active checkpoint contains body text")
		}
		if state.Phase == "digest" {
			var digest DocToolKeyDigestState
			if json.Unmarshal(checkpoint, &digest) != nil || bytes.Contains(digest.HashState, []byte("PUBLIC_BODY_MARKER")) {
				test.Fatal("digest checkpoint retained an unhashed public body tail")
			}
		}
	}
	fixture.state = state
	fixture.states = append(fixture.states, state)
	fixture.steps = append(fixture.steps, step)
	fixture.items = append(fixture.items, step.Items()...)
	fixture.rawBytes += rawBytes
	fixture.objectReads += reads
}

func (fixture *docToolKeyMapFixture) run(test testing.TB) {
	test.Helper()
	for !fixture.state.Done() {
		if len(fixture.steps) > len(fixture.tree.raw)+fixture.source.Nodes {
			test.Fatal("key-map worker did not terminate")
		}
		fixture.advance(test)
	}
	keys := 0
	for _, node := range fixture.tree.nodes {
		if node.Key != nil {
			keys++
		}
	}
	if fixture.state.Seen != len(fixture.tree.nodes) || fixture.state.Keys != keys || len(fixture.items) != keys || fixture.state.After != fixture.tree.nodes[len(fixture.tree.nodes)-1].Value.From {
		test.Fatal("completion lost nodes or emitted duplicate candidates", fixture.state, keys, len(fixture.items))
	}
}

type docToolKeyMapOracleItem struct {
	From         int         `json:"from"`
	Digest       ContentHash `json:"digest"`
	IsArrayIndex bool        `json:"numeric"`
	ArrayIndex   uint32      `json:"index"`
}

type docToolKeyMapOracleMember struct {
	docToolKeyMapOracleItem
	First int    `json:"first"`
	Last  int    `json:"last"`
	Raw   string `json:"raw"`
}

type docToolKeyMapOracleResult struct {
	Items   []docToolKeyMapOracleItem `json:"items"`
	Objects []struct {
		Parent  int                         `json:"parent"`
		Members []docToolKeyMapOracleMember `json:"members"`
	} `json:"objects"`
}

func docToolKeyMapJavaScriptOracle(test testing.TB, fixture *docToolKeyMapFixture) docToolKeyMapOracleResult {
	test.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		test.Fatal("actual local Node/crypto oracle is required", err)
	}
	type tokenInput struct {
		From   int    `json:"from"`
		Parent int    `json:"parent"`
		Raw    string `json:"raw"`
		Value  string `json:"value"`
	}
	type objectInput struct {
		Parent int    `json:"parent"`
		Raw    string `json:"raw"`
	}
	input := struct {
		Tokens  []tokenInput  `json:"tokens"`
		Objects []objectInput `json:"objects"`
	}{Tokens: []tokenInput{}, Objects: []objectInput{}}
	for _, node := range fixture.tree.nodes {
		value := string(fixture.tree.raw[node.Value.From:node.Value.To])
		if node.Key != nil {
			input.Tokens = append(input.Tokens, tokenInput{From: node.Value.From, Parent: node.Parent, Raw: string(fixture.tree.raw[node.Key.From:node.Key.To]), Value: value})
		}
		if node.Type == DocToolValueObject {
			input.Objects = append(input.Objects, objectInput{Parent: node.Value.From, Raw: value})
		}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", `
const fs = require('node:fs'), crypto = require('node:crypto');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const facts = key => {
  const index = key >>> 0, numeric = key === String(index) && index !== 4294967295;
  return {digest: 'sha256:' + crypto.createHash('sha256').update(JSON.stringify(key), 'utf8').digest('hex'), numeric, index: numeric ? index : 0};
};
const items = input.tokens.map(token => ({from: token.from, ...facts(JSON.parse(token.raw))}));
const objects = input.objects.map(object => {
  const value = JSON.parse(object.raw), tokens = input.tokens.filter(token => token.parent === object.parent);
  const members = Object.keys(value).map(key => {
    const matches = tokens.filter(token => JSON.parse(token.raw) === key), last = matches.at(-1);
    if (!last || JSON.stringify(JSON.parse(last.value)) !== JSON.stringify(value[key])) throw Error('last whole value was not selected');
    return {...facts(key), first: matches[0].from, last: last.from, raw: last.value};
  });
  return {parent: object.parent, members};
});
process.stdout.write(JSON.stringify({items, objects}));`)
	command.Stdin = bytes.NewReader(raw)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("local Node oracle failed: %v: %s", err, output)
	}
	var result docToolKeyMapOracleResult
	if err := json.Unmarshal(output, &result); err != nil || len(result.Items) != len(input.Tokens) || len(result.Objects) != len(input.Objects) {
		test.Fatal("invalid Node oracle response", err)
	}
	return result
}

func foldDocToolKeyMapItems(test testing.TB, tree DocToolTreeSource, items []DocToolKeyItem) []DocToolKeyMember {
	test.Helper()
	type identity struct {
		parent int
		digest ContentHash
	}
	groups := make(map[identity]DocToolKeyMember)
	for _, item := range items {
		key := identity{parent: item.Node.Parent, digest: item.Digest}
		var prior *DocToolKeyMember
		if existing, found := groups[key]; found {
			prior = &existing
		}
		member, err := MergeDocToolKeyMember(tree, prior, item)
		if err != nil || member.Validate(tree) != nil {
			test.Fatal("domain member fold failed", err)
		}
		groups[key] = member
	}
	members := make([]DocToolKeyMember, 0, len(groups))
	for _, member := range groups {
		members = append(members, member)
	}
	slices.SortFunc(members, CompareDocToolKeyMembers)
	return members
}

func assertDocToolKeyMapOracle(test testing.TB, fixture *docToolKeyMapFixture) {
	test.Helper()
	oracle := docToolKeyMapJavaScriptOracle(test, fixture)
	if len(oracle.Items) != len(fixture.items) {
		test.Fatal("candidate count differs from JavaScript")
	}
	for index, want := range oracle.Items {
		item := fixture.items[index]
		if item.Node.Value.From != want.From || item.Digest != want.Digest || item.IsArrayIndex != want.IsArrayIndex || item.ArrayIndex != want.ArrayIndex {
			test.Fatal("key digest or numeric policy differs from actual JavaScript", index, item, want)
		}
	}
	members := foldDocToolKeyMapItems(test, fixture.tree.source, fixture.items)
	matched := 0
	for _, object := range oracle.Objects {
		var actual []DocToolKeyMember
		for _, member := range members {
			if member.Last.Node.Parent == object.Parent {
				actual = append(actual, member)
			}
		}
		if len(actual) != len(object.Members) {
			test.Fatal("parent-scoped member count differs from Object.keys", object.Parent, len(actual), len(object.Members))
		}
		for index, want := range object.Members {
			got := actual[index]
			value := string(fixture.tree.raw[got.Last.Node.Value.From:got.Last.Node.Value.To])
			if got.First != want.First || got.Last.Node.Value.From != want.Last || got.Last.Digest != want.Digest || got.Last.IsArrayIndex != want.IsArrayIndex || got.Last.ArrayIndex != want.ArrayIndex || value != want.Raw {
				test.Fatal("Object.keys order, first insertion, or whole last original value differs from JavaScript", object.Parent, index, got, want)
			}
		}
		matched += len(actual)
	}
	if matched != len(members) {
		test.Fatal("member escaped its original object parent")
	}
	for _, order := range []string{"reversed", "replayed", "rotated"} {
		items := append([]DocToolKeyItem(nil), fixture.items...)
		switch order {
		case "reversed":
			slices.Reverse(items)
		case "replayed":
			items = append(items, items...)
		case "rotated":
			middle := len(items) / 2
			items = append(items[middle:], items[:middle]...)
		}
		if got := foldDocToolKeyMapItems(test, fixture.tree.source, items); !reflect.DeepEqual(got, members) {
			test.Fatal("domain member fold is not order-independent and idempotent", order)
		}
	}
}

func TestDocToolKeyMapMatchesLocalJavaScript(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		value string
	}{
		{"root-order-aliases-and-utf16", `{"z":0,"10":10,"2":2,"a":{"old":1,"shared":{"old":true}},"\u0061":{"new":2,"shared":{"new":true}},"z":[3],"0":0,"00":0,"01":1,"-0":0,"4294967294":4,"4294967295":5,"4294967296":6,"1.0":1,"1e0":1,"":null,"\uD800":1,"\ud800":2,"\ud801":3,"\udc00":4,"�":5,"\ufffd":6,"🙂":7,"\ud83d\ude42":8,"\u0032":22,"\u0000<&>\u2028\u2029\"\\\/":9,"__proto__":{"own":true},"constructor":false,"hasOwnProperty":true}`},
		{"nested-parent-scope-and-whole-replacement", `{"same":{"same":{"discard":1},"same":[2]},"other":{"same":3,"\u0073ame":{"keep":4}},"list":[{"same":5},{"same":6,"same":null}],"replace":{"left":true,"nested":{"old":1}},"tail":0,"\u0072eplace":{"right":false,"nested":{"new":2}}}`},
		{"array-parent-and-keyless-values", `[{"10":0,"2":1,"same":false,"same":{"only":true}},null,[],"PUBLIC_BODY_MARKER",0,true,false]`},
		{"empty-object", `{}`},
		{"primitive", `"PUBLIC_BODY_MARKER"`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newDocToolKeyMapFixture(test, scenario.value)
			fixture.run(test)
			assertDocToolKeyMapOracle(test, fixture)
		})
	}
}

func TestDocToolKeyMapShortKeysHaveNoPersistentStringPlans(test *testing.T) {
	var value strings.Builder
	value.WriteByte('{')
	for index := 0; index < 70; index++ {
		if index > 0 {
			value.WriteByte(',')
		}
		fmt.Fprintf(&value, `"key-%d":"PUBLIC_BODY_MARKER"`, index)
	}
	value.WriteString(`,"` + strings.Repeat("x", 126) + `":0,"` + strings.Repeat(`\u0061`, 21) + `":1,"\uD800":2,"\u0030":3}`)
	fixture := newDocToolKeyMapFixture(test, value.String())
	fixture.run(test)
	keyBytes := 0
	for _, node := range fixture.tree.nodes {
		if node.Key != nil {
			length := node.Key.To - node.Key.From
			if length > 128 {
				test.Fatal("fixture escaped the inline token boundary")
			}
			keyBytes += length
		}
	}
	if len(fixture.steps) != (fixture.source.Nodes+15)/16 || fixture.objectReads != 0 || fixture.rawBytes != keyBytes {
		test.Fatal("short keys used persistent plans, rereads, or undersized batches", len(fixture.steps), fixture.objectReads, fixture.rawBytes, keyBytes)
	}
	for _, step := range fixture.steps {
		if len(step.Objects()) != 0 || step.State().Current != nil || step.State().Active != "" || step.State().Plan != "" {
			test.Fatal("short keys emitted persistent string-plan objects")
		}
	}
	assertDocToolKeyMapOracle(test, fixture)
}

func TestDocToolKeyMapLongKeysResumeAndAliasInlineKeys(test *testing.T) {
	long := strings.Repeat("PUBLIC_BODY_MARKER", 4200)
	value := `{"before":0,"` + long + `\uD800\u0000🙂":{"discard":1},"middle":1,"` + long + `\ud800\u0000\ud83d\ude42":{"keep":2},"` + strings.Repeat("a", 22) + `":0,"` + strings.Repeat(`\u0061`, 22) + `":1,"` + strings.Repeat("x", 127) + `":2,"after":3}`
	fixture := newDocToolKeyMapFixture(test, value)
	fixture.run(test)
	stringsSeen, digestsSeen, objects, maximumRaw := 0, 0, 0, 0
	for index, step := range fixture.steps {
		objects += len(step.Objects())
		maximumRaw = max(maximumRaw, step.ReadBytes())
		switch fixture.states[index].Phase {
		case "string":
			stringsSeen++
		case "digest":
			digestsSeen++
			if step.ReadBytes() != 0 {
				test.Fatal("digest phase reread event bytes")
			}
		}
	}
	if stringsSeen < 4 || digestsSeen < 4 || objects == 0 || fixture.objectReads == 0 || maximumRaw == 0 {
		test.Fatal("long-key fixture did not exercise persistent multi-step string and digest restart", stringsSeen, digestsSeen, objects)
	}
	assertDocToolKeyMapOracle(test, fixture)
	test.Logf("nodes=%d keys=%d steps=%d string_steps=%d digest_steps=%d objects=%d max_raw=%d", fixture.source.Nodes, len(fixture.items), len(fixture.steps), stringsSeen, digestsSeen, objects, maximumRaw)
}

func TestDocToolKeyMapStepGettersDoNotAlias(test *testing.T) {
	fixture := newDocToolKeyMapFixture(test, `{"short":0,"`+strings.Repeat("PUBLIC_BODY_MARKER", 9)+`":1,"tail":2}`)
	fixture.advance(test)
	step := fixture.steps[0]
	snapshot := func() []byte {
		raw, err := json.Marshal(struct {
			State   DocToolKeyMapState
			Inputs  []DocToolTreeNode
			Items   []DocToolKeyItem
			Objects []DocRangeObject
		}{State: step.State(), Inputs: step.Inputs(), Items: step.Items(), Objects: step.Objects()})
		if err != nil {
			test.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	state, inputs, items, objects := step.State(), step.Inputs(), step.Items(), step.Objects()
	if state.Current == nil || state.Current.Key == nil || len(items) == 0 || len(objects) == 0 {
		test.Fatal("fixture did not produce all sealed getter payloads")
	}
	state.Current.Key.From++
	state.Current.Value.From++
	state.Active = HashContent([]byte("changed"))
	inputs[1].Key.From++
	inputs[1].Value.From++
	items[0].Node.Key.From++
	items[0].Digest = HashContent([]byte("changed"))
	objects[0].Bytes[0] ^= 1
	objects[0].Hash = HashContent([]byte("changed"))
	if !bytes.Equal(before, snapshot()) || !reflect.DeepEqual(step.State(), fixture.states[1]) {
		test.Fatal("getter exposed mutable internal state")
	}
	for _, node := range fixture.tree.nodes {
		if node.Key != nil {
			node.Key.From++
		}
	}
	if !bytes.Equal(before, snapshot()) || !reflect.DeepEqual(step.State(), fixture.states[1]) {
		test.Fatal("loader-owned node pointers alias a sealed step")
	}
	stepType := reflect.TypeOf(step)
	for index := 0; index < stepType.NumField(); index++ {
		if stepType.Field(index).IsExported() {
			test.Fatal("step has an unsealed mutable public field", stepType.Field(index).Name)
		}
	}
}

func TestDocToolKeyMemberMergeRejectsMismatchWithoutAliasing(test *testing.T) {
	fixture := newDocToolKeyMapFixture(test, `{"a":{"old":true},"nested":{"a":0},"a":{"last":false},"0":0,"1":1}`)
	fixture.run(test)
	var aliases []DocToolKeyItem
	for _, item := range fixture.items {
		if item.Node.Parent == fixture.tree.source.Root.From && string(fixture.tree.raw[item.Node.Key.From:item.Node.Key.To]) == `"a"` {
			aliases = append(aliases, item)
		}
	}
	if len(aliases) != 2 {
		test.Fatal("bad duplicate fixture")
	}
	prior, err := MergeDocToolKeyMember(fixture.tree.source, nil, aliases[0])
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*DocToolKeyItem)
	}{
		{"digest", func(item *DocToolKeyItem) { item.Digest = HashContent([]byte("different")) }},
		{"index-class", func(item *DocToolKeyItem) { item.IsArrayIndex, item.ArrayIndex = true, 0 }},
		{"invalid-index", func(item *DocToolKeyItem) { item.ArrayIndex = 1 }},
		{"parent", func(item *DocToolKeyItem) {
			for _, candidate := range fixture.items {
				if candidate.Node.Parent != item.Node.Parent && candidate.Digest == item.Digest {
					*item = candidate
					return
				}
			}
			test.Fatal("missing nested duplicate")
		}},
		{"same-position-different-node", func(item *DocToolKeyItem) { *item = aliases[0]; item.Node.Children = 0 }},
		{"invalid-key", func(item *DocToolKeyItem) { item.Node.Key = nil }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			item := aliases[1]
			scenario.change(&item)
			beforePrior, _ := json.Marshal(prior)
			beforeItem, _ := json.Marshal(item)
			result, err := MergeDocToolKeyMember(fixture.tree.source, &prior, item)
			afterPrior, _ := json.Marshal(prior)
			afterItem, _ := json.Marshal(item)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocToolKeyMember{}) || !bytes.Equal(beforePrior, afterPrior) || !bytes.Equal(beforeItem, afterItem) {
				test.Fatal("mismatched member merged or failure mutated caller-owned values", err)
			}
		})
	}
	result, err := MergeDocToolKeyMember(fixture.tree.source, &prior, aliases[1])
	if err != nil || result.First != aliases[0].Node.Value.From || !reflect.DeepEqual(result.Last, aliases[1]) || string(fixture.tree.raw[result.Last.Node.Value.From:result.Last.Node.Value.To]) != `{"last":false}` {
		test.Fatal("merge did not preserve first insertion and whole last original value", err)
	}
	result.Last.Node.Key.From++
	if result.Last.Node.Key.From == aliases[1].Node.Key.From || !reflect.DeepEqual(prior.Last, aliases[0]) {
		test.Fatal("merge result aliases incoming or previous key span")
	}
	result, err = MergeDocToolKeyMember(fixture.tree.source, &prior, aliases[0])
	if err != nil {
		test.Fatal(err)
	}
	result.Last.Node.Key.From++
	if !reflect.DeepEqual(prior.Last, aliases[0]) || result.Last.Node.Key.From == prior.Last.Node.Key.From {
		test.Fatal("idempotent merge aliases prior")
	}
	initial, err := MergeDocToolKeyMember(fixture.tree.source, nil, aliases[1])
	if err != nil {
		test.Fatal(err)
	}
	initial.Last.Node.Key.From++
	if initial.Last.Node.Key.From == aliases[1].Node.Key.From {
		test.Fatal("initial merge aliases incoming key")
	}
	for _, item := range fixture.items {
		if !item.IsArrayIndex {
			continue
		}
		numeric, err := MergeDocToolKeyMember(fixture.tree.source, nil, item)
		if err != nil {
			test.Fatal(err)
		}
		changed := item
		changed.ArrayIndex++
		merged, err := MergeDocToolKeyMember(fixture.tree.source, &numeric, changed)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(merged, DocToolKeyMember{}) || !reflect.DeepEqual(numeric.Last, item) {
			test.Fatal("same digest with different numeric classification was merged", err)
		}
		invalid := numeric
		invalid.First = item.Node.Parent
		merged, err = MergeDocToolKeyMember(fixture.tree.source, &invalid, item)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(merged, DocToolKeyMember{}) {
			test.Fatal("invalid prior member was merged", err)
		}
	}
}

func TestDocToolKeyMemberComparisonContract(test *testing.T) {
	ordinary := DocToolKeyMember{First: 10, Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, Digest: ContentHash("b")}}
	for _, scenario := range []struct {
		name  string
		left  DocToolKeyMember
		right DocToolKeyMember
	}{
		{"parent-before-class", DocToolKeyMember{Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 0}}}, DocToolKeyMember{Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, IsArrayIndex: true}}},
		{"indices-before-insertion", DocToolKeyMember{First: 100, Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, IsArrayIndex: true, ArrayIndex: 10}}, ordinary},
		{"numeric-not-lexical", DocToolKeyMember{First: 100, Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, IsArrayIndex: true, ArrayIndex: 2}}, DocToolKeyMember{First: 1, Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, IsArrayIndex: true, ArrayIndex: 10}}},
		{"ordinary-first-insertion", DocToolKeyMember{First: 9, Last: ordinary.Last}, ordinary},
		{"digest-tie-break", DocToolKeyMember{First: 10, Last: DocToolKeyItem{Node: DocToolTreeNode{Parent: 1}, Digest: ContentHash("a")}}, ordinary},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if CompareDocToolKeyMembers(scenario.left, scenario.right) >= 0 || CompareDocToolKeyMembers(scenario.right, scenario.left) <= 0 || CompareDocToolKeyMembers(scenario.left, scenario.left) != 0 {
				test.Fatal("member comparator violates ordering, antisymmetry, or equality")
			}
		})
	}
}

func assertDocToolKeyMapFailure(test testing.TB, fixture *docToolKeyMapFixture, state DocToolKeyMapState, ctx context.Context, raw func(context.Context, int, int) ([]byte, error), objects func(context.Context, ContentHash) ([]byte, error), nodes DocToolTreeNodeLoader, expected error) {
	test.Helper()
	beforeState, _ := json.Marshal(state)
	beforeObjects, _ := json.Marshal(fixture.tree.objects)
	step, err := StepDocToolKeyMap(ctx, fixture.tree.source, fixture.source, state, raw, objects, nodes)
	afterState, _ := json.Marshal(state)
	afterObjects, _ := json.Marshal(fixture.tree.objects)
	if !errors.Is(err, expected) || !reflect.DeepEqual(step, DocToolKeyMapStep{}) || !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeObjects, afterObjects) {
		test.Fatal("failure leaked effects or mutated predecessor/persisted objects", err, expected)
	}
}

func TestDocToolKeyMapReadAndNodeFailuresHaveZeroEffects(test *testing.T) {
	fixture := newDocToolKeyMapFixture(test, `{"first":0,"second":{"nested":1},"last":2}`)
	failure := errors.New("injected key-map I/O failure")
	for _, scenario := range []string{"raw-io-after-prefix", "short-raw-after-prefix", "invalid-raw-after-prefix", "invalid-utf8", "node-io", "missing-page", "short-page", "oversized-page", "unordered-page", "invalid-node", "missing-root", "nil-raw", "nil-objects", "nil-nodes", "cancel-before", "cancel-raw", "cancel-nodes"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rawReads, objectReads, nodeReads := 0, 0, 0
			expected := error(ErrIntegrity)
			raw := func(ctx context.Context, from, length int) ([]byte, error) {
				rawReads++
				value, err := fixture.tree.read(ctx, from, length)
				if rawReads == 2 {
					switch scenario {
					case "raw-io-after-prefix":
						return nil, failure
					case "short-raw-after-prefix":
						return value[:len(value)-1], nil
					case "invalid-raw-after-prefix":
						value[0] = 'x'
					case "invalid-utf8":
						value[1] = 0xff
					}
				}
				if scenario == "cancel-raw" {
					cancel()
				}
				return value, err
			}
			objects := func(ctx context.Context, hash ContentHash) ([]byte, error) {
				objectReads++
				return fixture.tree.load(ctx, hash)
			}
			nodes := func(ctx context.Context, after, limit int) ([]DocToolTreeNode, error) {
				nodeReads++
				page, err := fixture.loadNodes(ctx, after, limit)
				switch scenario {
				case "node-io":
					return nil, failure
				case "missing-page":
					return nil, nil
				case "short-page":
					page = page[:len(page)-1]
				case "oversized-page":
					page = append(page, page[len(page)-1])
				case "unordered-page":
					page[1], page[2] = page[2], page[1]
				case "invalid-node":
					page[1].Parent = -1
				case "missing-root":
					page[0] = page[1]
				case "cancel-nodes":
					cancel()
				}
				return page, err
			}
			switch scenario {
			case "raw-io-after-prefix", "node-io":
				expected = failure
			case "nil-raw":
				raw = nil
			case "nil-objects":
				objects = nil
			case "nil-nodes":
				nodes = nil
			case "cancel-before":
				cancel()
				expected = context.Canceled
			case "cancel-raw", "cancel-nodes":
				expected = context.Canceled
			}
			assertDocToolKeyMapFailure(test, fixture, fixture.state, ctx, raw, objects, nodes, expected)
			if scenario == "cancel-before" || strings.HasPrefix(scenario, "nil-") {
				if rawReads+objectReads+nodeReads != 0 {
					test.Fatal("invalid admission performed I/O")
				}
			}
		})
	}
}

func TestDocToolKeyMapLongPlanFailuresHaveZeroEffects(test *testing.T) {
	fixture := newDocToolKeyMapFixture(test, `{"prefix":0,"`+strings.Repeat("PUBLIC_BODY_MARKER", 4100)+`":1,"tail":2}`)
	fixture.run(test)
	failure := errors.New("injected long-plan I/O failure")
	for _, phase := range []string{"string", "digest"} {
		var state DocToolKeyMapState
		for _, candidate := range fixture.states {
			if candidate.Phase == phase {
				state = candidate
				break
			}
		}
		if state.Current == nil {
			test.Fatal("missing resumable long-key phase", phase)
		}
		for _, scenario := range []string{"object-io", "missing-active", "corrupt-active", "short-node-page", "changed-current-node", "cancel-object", "cancel-raw", "raw-io", "short-raw", "corrupt-plan"} {
			if phase == "digest" && (scenario == "cancel-raw" || scenario == "raw-io" || scenario == "short-raw") || phase == "string" && scenario == "corrupt-plan" {
				continue
			}
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads := 0
				expected := error(ErrIntegrity)
				raw := func(ctx context.Context, from, length int) ([]byte, error) {
					reads++
					if scenario == "cancel-raw" {
						cancel()
					}
					if scenario == "raw-io" {
						return nil, failure
					}
					value, err := fixture.tree.read(ctx, from, length)
					if scenario == "short-raw" {
						return value[:len(value)-1], nil
					}
					return value, err
				}
				objects := func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if scenario == "object-io" {
						return nil, failure
					}
					if scenario == "missing-active" && hash == state.Active {
						return nil, ErrNotFound
					}
					value, err := fixture.tree.load(ctx, hash)
					if scenario == "corrupt-active" && hash == state.Active || scenario == "corrupt-plan" && hash == state.Plan {
						return append(value, ' '), nil
					}
					if scenario == "cancel-object" {
						cancel()
					}
					return value, err
				}
				nodes := func(ctx context.Context, after, limit int) ([]DocToolTreeNode, error) {
					page, err := fixture.loadNodes(ctx, after, limit)
					if scenario == "short-node-page" {
						return nil, nil
					}
					if scenario == "changed-current-node" {
						page[0].Ordinal++
					}
					return page, err
				}
				switch scenario {
				case "object-io", "raw-io":
					expected = failure
				case "missing-active":
					expected = ErrNotFound
				case "cancel-object", "cancel-raw":
					expected = context.Canceled
				}
				assertDocToolKeyMapFailure(test, fixture, state, ctx, raw, objects, nodes, expected)
				if scenario != "cancel-raw" && scenario != "raw-io" && scenario != "short-raw" && reads != 0 {
					test.Fatal("corrupt checkpoint or failed node page reached raw source")
				}
			})
		}
	}
}

func TestDocToolKeyMapResumedObjectsAreSourceBound(test *testing.T) {
	value := `{"` + strings.Repeat("PUBLIC_BODY_MARKER", 8200) + `":0}`
	fixture := newDocToolKeyMapFixture(test, value)
	foreign := newDocToolKeyMapFixture(test, strings.TrimSuffix(value, "0}")+"1}")
	fixture.run(test)
	foreign.run(test)
	for _, phase := range []string{"string", "digest"} {
		var state, other DocToolKeyMapState
		for _, candidate := range fixture.states {
			if candidate.Phase != phase {
				continue
			}
			if phase == "string" {
				var inner DocToolStringState
				if json.Unmarshal(fixture.tree.objects[candidate.Active], &inner) != nil || inner.Directory.Count == 0 {
					continue
				}
			}
			state = candidate
			break
		}
		for _, candidate := range foreign.states {
			if candidate.Phase == phase {
				other = candidate
				break
			}
		}
		if state.Current == nil || other.Current == nil {
			test.Fatal("missing restart state", phase)
		}
		for _, scenario := range []string{"foreign-active", "rehashed-active", "foreign-plan", "rehashed-plan", "corrupt-persisted-page"} {
			if phase == "string" && strings.HasSuffix(scenario, "plan") {
				continue
			}
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				changed := state
				injected := make(map[ContentHash][]byte)
				put := func(raw []byte, err error) ContentHash {
					if err != nil {
						test.Fatal(err)
					}
					hash := HashContent(raw)
					injected[hash] = raw
					return hash
				}
				switch scenario {
				case "foreign-active":
					changed.Active = put(foreign.tree.objects[other.Active], nil)
				case "foreign-plan":
					changed.Plan = put(foreign.tree.objects[other.Plan], nil)
				case "rehashed-active":
					if phase == "string" {
						var inner DocToolStringState
						if err := json.Unmarshal(fixture.tree.objects[state.Active], &inner); err != nil {
							test.Fatal(err)
						}
						inner.Source = HashContent([]byte("another string source"))
						changed.Active = put(canonicalJSON(inner))
					} else {
						var inner DocToolKeyDigestState
						if err := json.Unmarshal(fixture.tree.objects[state.Active], &inner); err != nil {
							test.Fatal(err)
						}
						inner.Plan = HashContent([]byte("another digest plan"))
						changed.Active = put(canonicalJSON(inner))
					}
				case "rehashed-plan":
					plan, err := DecodeDocToolStringPlan(state.Plan, fixture.tree.objects[state.Plan])
					if err != nil {
						test.Fatal(err)
					}
					plan.Source.Mode = DocToolStringAdd
					changed.Plan = put(plan.Encode())
				case "corrupt-persisted-page":
					var leaf ContentHash
					if phase == "string" {
						var inner DocToolStringState
						if err := json.Unmarshal(fixture.tree.objects[state.Active], &inner); err != nil {
							test.Fatal(err)
						}
						leaf = inner.Directory.Levels[0][len(inner.Directory.Levels[0])-1]
					} else {
						plan, err := DecodeDocToolStringPlan(state.Plan, fixture.tree.objects[state.Plan])
						if err != nil {
							test.Fatal(err)
						}
						leaf, err = ResolveDocRangeLeaf(context.Background(), plan.Pages, 0, fixture.tree.load)
						if err != nil {
							test.Fatal(err)
						}
					}
					injected[leaf] = append(bytes.Clone(fixture.tree.objects[leaf]), ' ')
				}
				reads := 0
				assertDocToolKeyMapFailure(test, fixture, changed, context.Background(), func(ctx context.Context, from, length int) ([]byte, error) {
					reads++
					return fixture.tree.read(ctx, from, length)
				}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if raw, found := injected[hash]; found {
						return bytes.Clone(raw), nil
					}
					return fixture.tree.load(ctx, hash)
				}, fixture.loadNodes, ErrIntegrity)
				if reads != 0 {
					test.Fatal("corrupt or foreign resumed object reached raw source")
				}
			})
		}
	}
}

func TestDocToolKeyMapSourceAndCanonicalCheckpointBinding(test *testing.T) {
	fixture := newDocToolKeyMapFixture(test, `{"a":0}`)
	if _, err := NewDocToolKeyMapSource(fixture.tree.source, fixture.tree.states[0]); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unfinished tree admitted", err)
	}
	other := newDocToolKeyMapFixture(test, `{"b":1}`)
	if fixture.source.Validate(other.tree.source) == nil {
		test.Fatal("map source accepted another tree")
	}
	reads := 0
	step, err := StepDocToolKeyMap(context.Background(), fixture.tree.source, other.source, fixture.state,
		func(context.Context, int, int) ([]byte, error) { reads++; return nil, nil },
		func(context.Context, ContentHash) ([]byte, error) { reads++; return nil, nil },
		func(context.Context, int, int) ([]DocToolTreeNode, error) { reads++; return nil, nil })
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyMapStep{}) || reads != 0 {
		test.Fatal("foreign key-map source performed I/O or leaked effects", err)
	}
	for _, mutate := range []func(*DocToolKeyMapSource){
		func(source *DocToolKeyMapSource) { source.Version++ },
		func(source *DocToolKeyMapSource) { source.Profile = DocToolTreeProfile },
		func(source *DocToolKeyMapSource) { source.Tree = other.source.Tree },
		func(source *DocToolKeyMapSource) { source.Nodes = 0 },
		func(source *DocToolKeyMapSource) {
			source.Nodes = fixture.tree.source.Root.To - fixture.tree.source.Root.From + 1
		},
	} {
		source := fixture.source
		mutate(&source)
		if source.Validate(fixture.tree.source) == nil {
			test.Fatal("invalid key-map source admitted", source)
		}
	}
	encoded, err := fixture.state.Encode(fixture.tree.source, fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	for _, raw := range [][]byte{nil, append([]byte(" "), encoded...), append(append(bytes.Clone(encoded[:len(encoded)-1]), []byte(`,"unexpected":true`)...), '}'), append(bytes.Clone(encoded), ' '), []byte(strings.Repeat(" ", (64<<10)+1))} {
		if _, err := DecodeDocToolKeyMapState(fixture.tree.source, fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical checkpoint accepted", err)
		}
	}
	if _, err := DecodeDocToolKeyMapState(fixture.tree.source, fixture.source, HashContent([]byte("wrong")), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatal("checkpoint digest was not bound", err)
	}
	for _, mutate := range []func(*DocToolKeyMapState){
		func(state *DocToolKeyMapState) { state.Version++ },
		func(state *DocToolKeyMapState) { state.Source = other.state.Source },
		func(state *DocToolKeyMapState) { state.Phase = "unknown" },
		func(state *DocToolKeyMapState) { state.Phase = "done" },
		func(state *DocToolKeyMapState) { state.Seen = -1 },
		func(state *DocToolKeyMapState) { state.Keys = 1 },
		func(state *DocToolKeyMapState) { state.After++ },
		func(state *DocToolKeyMapState) { state.Active = HashContent([]byte("unexpected")) },
		func(state *DocToolKeyMapState) { state.Plan = HashContent([]byte("unexpected")) },
	} {
		state := fixture.state
		mutate(&state)
		reads := 0
		assertDocToolKeyMapFailure(test, fixture, state, context.Background(), func(context.Context, int, int) ([]byte, error) { reads++; return nil, nil }, func(context.Context, ContentHash) ([]byte, error) { reads++; return nil, nil }, func(context.Context, int, int) ([]DocToolTreeNode, error) { reads++; return nil, nil }, ErrIntegrity)
		if reads != 0 {
			test.Fatal("invalid source/checkpoint performed I/O")
		}
	}
	fixture.run(test)
	assertDocToolKeyMapFailure(test, fixture, fixture.state, context.Background(), fixture.tree.read, fixture.tree.load, fixture.loadNodes, ErrIntegrity)
}
