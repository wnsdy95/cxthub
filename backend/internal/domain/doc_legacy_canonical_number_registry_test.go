package domain

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalNumberRegistryShape(test testing.TB, registry *legacyCanonicalNumberRegistry) (int, int, int) {
	test.Helper()
	nodes, entries, branches, deepest := 0, 0, 0, 0
	var expected docRangeHeaderHeapEstimate
	var path [legacyCanonicalNumberDigits]int
	var visit func(*legacyCanonicalNumberRegistryNode, int)
	visit = func(node *legacyCanonicalNumberRegistryNode, depth int) {
		if node == nil {
			return
		}
		if depth > legacyCanonicalNumberDigits {
			test.Fatal("registry lookup exceeded its finite pointer and length digits", depth)
		}
		nodes++
		expected.allocation(1, int64(unsafe.Sizeof(*node)))
		deepest = max(deepest, depth)
		if node.branched {
			branches++
			if depth == legacyCanonicalNumberDigits || node.used != 0 || node.entries != ([legacyCanonicalNumberBucketValues]legacyCanonicalNumberEntry{}) {
				test.Fatal("split registry bucket retained entries or exceeded its radix depth", depth, node.used)
			}
			children := 0
			for digit, child := range node.children {
				if child != nil {
					children++
					path[depth] = digit
					visit(child, depth+1)
				}
			}
			if children == 0 {
				test.Fatal("registry allocated an empty branch")
			}
			return
		}
		if node.used <= 0 || node.used > legacyCanonicalNumberBucketValues || node.children != ([legacyCanonicalNumberRadix]*legacyCanonicalNumberRegistryNode{}) {
			test.Fatal("registry leaf escaped its fixed bucket", node.used)
		}
		entries += node.used
		for position, entry := range node.entries {
			if position >= node.used {
				if entry != (legacyCanonicalNumberEntry{}) {
					test.Fatal("registry retained an unused identity", position)
				}
				continue
			}
			for level := 0; level < depth; level++ {
				if legacyCanonicalNumberDigit(entry.identity, level) != path[level] {
					test.Fatal("registry split routed an identity into the wrong bucket", depth, level)
				}
			}
		}
	}
	visit(registry.root, 0)
	if registry.length() != entries || registry.nodes != nodes {
		test.Fatal("registry allocation counters disagree with reachable storage", registry.length(), entries, registry.nodes, nodes)
	}
	var actual docRangeHeaderHeapEstimate
	registry.retained(&actual)
	if expected.err != nil || actual.err != nil || actual.bytes != expected.bytes {
		test.Fatal("registry retained estimate is not its actual fixed-node allocation ledger", actual, expected)
	}
	return nodes, branches, deepest
}

func TestLegacyCanonicalNumberRegistryFullBucketSplitAndStatus(test *testing.T) {
	var marker byte
	var registry legacyCanonicalNumberRegistry
	for size := 0; size < legacyCanonicalNumberBucketValues; size++ {
		identity := legacyCanonicalNumberIdentity{start: &marker, size: size}
		inserted, err := registry.insert(identity)
		if err != nil || !inserted || registry.nodes != 1 || registry.root.branched {
			test.Fatal("registry grew before filling its lazy eight-entry bucket", size, registry.nodes, err)
		}
		if size%2 == 0 {
			if err := registry.prove(identity); err != nil {
				test.Fatal(err)
			}
		}
	}
	identity := legacyCanonicalNumberIdentity{start: &marker, size: legacyCanonicalNumberBucketValues}
	before, beforeRoot := registry, *registry.root
	work, err := registry.insertBytes(identity)
	repeated, repeatedErr := registry.insertBytes(identity)
	if err != nil || repeatedErr != nil || work != repeated || work <= 0 || work > 64<<10 || registry != before || *registry.root != beforeRoot {
		test.Fatal("registry split admission mutated storage or exceeded its original quantum", work, repeated, err, repeatedErr)
	}
	inserted, err := registry.insert(identity)
	if err != nil || !inserted {
		test.Fatal("registry rejected its admitted full-prefix split", err)
	}
	nodes, branches, deepest := legacyCanonicalNumberRegistryShape(test, &registry)
	if branches != legacyCanonicalNumberDigits || deepest != legacyCanonicalNumberDigits || nodes != legacyCanonicalNumberDigits+legacyCanonicalNumberBucketValues+1 {
		test.Fatal("full-prefix split did not stop at exact pointer and length digits", nodes, branches, deepest)
	}
	allocation := (nodes-before.nodes)*int(unsafe.Sizeof(legacyCanonicalNumberRegistryNode{})) +
		2*branches*int(unsafe.Sizeof([legacyCanonicalNumberBucketValues]legacyCanonicalNumberEntry{}))
	if work <= allocation {
		test.Fatal("split admission omitted bounded lookup and insertion work", work, allocation)
	}
	for size := 0; size <= legacyCanonicalNumberBucketValues; size++ {
		identity := legacyCanonicalNumberIdentity{start: &marker, size: size}
		proven, exists := registry.get(identity)
		if !exists || proven != (size < legacyCanonicalNumberBucketValues && size%2 == 0) {
			test.Fatal("split lost exact identity or pending/proven status", size, exists, proven)
		}
		before := registry
		inserted, err := registry.insert(identity)
		if err != nil || inserted || registry != before {
			test.Fatal("exact identity dedup changed count or allocations", size, inserted, err)
		}
	}
	if err := registry.prove(legacyCanonicalNumberIdentity{start: &marker, size: 9}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("proving an unregistered identity silently inserted it", err)
	}
}

func TestLegacyCanonicalNumberRegistryUsesOnlyPointerAndLength(test *testing.T) {
	original := strings.Clone("123456")
	clone := strings.Clone(original)
	if unsafe.StringData(original) == unsafe.StringData(clone) {
		test.Fatal("equal-text fixture reused its backing")
	}
	identities := []legacyCanonicalNumberIdentity{
		legacyCanonicalNumberKey(original),
		legacyCanonicalNumberKey(clone),
		legacyCanonicalNumberKey(original[:len(original)-1]),
		{start: unsafe.StringData(original), size: 0},
		{},
		{start: unsafe.StringData(original), size: int(^uint(0) >> 1)},
	}
	var registry legacyCanonicalNumberRegistry
	for _, identity := range identities {
		inserted, err := registry.insert(identity)
		if err != nil || !inserted {
			test.Fatal("registry hashed content, normalized empty pointers, or scanned an identity's bytes", identity.size, err)
		}
		if err := registry.prove(identity); err != nil {
			test.Fatal(err)
		}
	}
	if registry.length() != len(identities) {
		test.Fatal("distinct exact identities were deduplicated", registry.length())
	}
	for _, identity := range identities {
		if proven, exists := registry.get(identity); !exists || !proven {
			test.Fatal("registry did not retain exact pointer and length status", identity.size)
		}
	}
	legacyCanonicalNumberRegistryShape(test, &registry)
	registry.clear()
	legacyCanonicalNumberRegistryShape(test, &registry)
	if registry != (legacyCanonicalNumberRegistry{}) {
		test.Fatal("registry clear retained structural owners")
	}
}

func TestLegacyCanonicalNumberRegistrationPagedAdmissionAndRelease(test *testing.T) {
	var numbers legacyCanonicalNumberValidation
	for index := 0; index < 4097; index++ {
		text := strconv.Itoa(index + 1)
		before := numbers
		work, err := numbers.prepareBytes(text)
		repeated, repeatedErr := numbers.prepareBytes(text)
		registryWork, registryErr := numbers.proven.insertBytes(legacyCanonicalNumberKey(text))
		pendingWork, pendingErr := numbers.pending.appendBytes()
		if err != nil || repeatedErr != nil || registryErr != nil || pendingErr != nil || work != repeated ||
			work != registryWork+pendingWork || work <= 0 || work > 64<<10 || numbers != before {
			test.Fatal("registration did not plan unchanged bounded registry and page work", index, work, repeated, err)
		}
		if err := numbers.prepare(text); err != nil {
			test.Fatal("admitted registration failed", index, err)
		}
		if numbers.pending.length() != index+1 || numbers.proven.length() != index+1 || *numbers.pending.at(index) != text {
			test.Fatal("registration lost a number while growing fixed pages", index)
		}
		before = numbers
		duplicateWork, err := numbers.prepareBytes(text)
		if err != nil || duplicateWork <= 0 || duplicateWork > 64<<10 {
			test.Fatal("duplicate registration hid its bounded lookup work", duplicateWork, err)
		}
		if err := numbers.prepare(text); err != nil || numbers != before {
			test.Fatal("duplicate registration grew pending or registry storage", index, err)
		}
		switch index + 1 {
		case 8, 9, 64, 65, 4096, 4097:
			legacyCanonicalNumberRegistryShape(test, &numbers.proven)
			expected := legacyCanonicalIndependentNumberHeap(test, &numbers)
			var actual docRangeHeaderHeapEstimate
			numbers.retained(&actual)
			if actual.err != nil || expected.err != nil || actual.bytes != expected.bytes {
				test.Fatal("registration charged aliased numeric text or missed fixed allocations", index, actual, expected)
			}
		}
	}
	numbers.pending.clear()
	numbers.proven.clear()
	var released docRangeHeaderHeapEstimate
	numbers.retained(&released)
	if released.err != nil || released.bytes != 0 || numbers != (legacyCanonicalNumberValidation{}) {
		test.Fatal("cleared numeric registration retained pages or radix nodes", released)
	}
}

func TestLegacyCanonicalNumberRegistrationAdmissionFailureIsAtomic(test *testing.T) {
	for _, fixture := range []string{"pending-shape", "registry-overflow"} {
		test.Run(fixture, func(test *testing.T) {
			var numbers legacyCanonicalNumberValidation
			if err := numbers.prepare("1"); err != nil {
				test.Fatal(err)
			}
			if fixture == "pending-shape" {
				numbers.pending.count = -1
			} else {
				numbers.proven.nodes = int(^uint(0) >> 1)
			}
			before, beforeRoot := numbers, *numbers.proven.root
			if _, err := numbers.prepareBytes("2"); !errors.Is(err, ErrIntegrity) {
				test.Fatal("planning accepted invalid metadata arithmetic", err)
			}
			if err := numbers.prepare("2"); !errors.Is(err, ErrIntegrity) || numbers != before || *numbers.proven.root != beforeRoot {
				test.Fatal("failed actual admission partially committed an identity or pending value", err)
			}
			encoding := &LegacyCanonicalEncoding{numbers: numbers, eagerNumbers: true}
			if err := encoding.prepareNumber(context.Background(), "2"); !errors.Is(err, ErrIntegrity) || encoding.numbers != before {
				test.Fatal("eager wrapper did not propagate an unchanged admission failure", err)
			}
		})
	}
}

type legacyCanonicalNumberRegistryCommitContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalNumberRegistryCommitContext) Err() error {
	ctx.checks++
	if ctx.checks == 2 {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyCanonicalNumberRegistryProofDefersWithoutResettingPosition(test *testing.T) {
	text := strings.Repeat("1", 64<<10)
	var numbers legacyCanonicalNumberValidation
	if err := numbers.prepare(text); err != nil {
		test.Fatal(err)
	}
	beforeNodes, beforePages := numbers.proven.nodes, numbers.pending.pages
	complete, err := numbers.step(context.Background())
	if err != nil || complete || numbers.next != 0 || numbers.position != len(text) || numbers.valid(text) {
		test.Fatal("proof lookup escaped the shared character quantum or reset a pending commit", complete, numbers.next, numbers.position, err)
	}
	ctx := &legacyCanonicalNumberRegistryCommitContext{Context: context.Background()}
	complete, err = numbers.step(ctx)
	if complete || !errors.Is(err, context.Canceled) || numbers.next != 0 || numbers.position != len(text) || numbers.valid(text) {
		test.Fatal("deferred numeric proof committed before its context fence", complete, numbers.next, numbers.position, err)
	}
	complete, err = numbers.step(context.Background())
	if err != nil || !complete || numbers.next != 1 || numbers.position != 0 || !numbers.valid(text) ||
		numbers.proven.nodes != beforeNodes || numbers.pending.pages != beforePages {
		test.Fatal("deferred proof failed to finish without allocation", complete, numbers.next, numbers.position, err)
	}
}

func TestLegacyCanonicalNumberRegistryProofSharesByteAndItemLimits(test *testing.T) {
	var numbers legacyCanonicalNumberValidation
	offsets := make([]int, 1028)
	for index := 0; index < 1027; index++ {
		text := strconv.Itoa(index + 1)
		offsets[index+1] = offsets[index] + len(text)
		if err := numbers.prepare(text); err != nil {
			test.Fatal(err)
		}
	}
	beforeNodes, beforePages := numbers.proven.nodes, numbers.pending.pages
	for calls := 0; calls < 1027; calls++ {
		beforeNext, beforePosition := numbers.next, numbers.position
		complete, err := numbers.step(context.Background())
		marked := numbers.next - beforeNext
		characters := offsets[numbers.next] + numbers.position - offsets[beforeNext] - beforePosition
		if err != nil || marked < 0 || marked > 1024 || characters < 0 || characters+marked*legacyCanonicalNumberProofBytes > 64<<10 ||
			marked == 0 && numbers.position <= beforePosition || numbers.proven.nodes != beforeNodes || numbers.pending.pages != beforePages {
			test.Fatal("numeric proof exceeded shared byte/item work, stalled, or allocated", calls, characters, marked, err)
		}
		if calls == 0 && complete {
			test.Fatal("allocation-free proof lookups bypassed the metadata byte reservation")
		}
		if complete {
			for index := 0; index < numbers.pending.length(); index++ {
				if !numbers.valid(*numbers.pending.at(index)) {
					test.Fatal("completed registry retained an unproven numeric identity", index)
				}
			}
			return
		}
	}
	test.Fatal("bounded numeric registry proof did not terminate")
}
