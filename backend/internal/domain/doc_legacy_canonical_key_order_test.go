package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

type legacyCanonicalKeyFieldsForTest []legacyCanonicalKeyField

func (fields legacyCanonicalKeyFieldsForTest) length() int {
	return len(fields)
}

func (fields legacyCanonicalKeyFieldsForTest) at(position int) *legacyCanonicalKeyField {
	if position < 0 || position >= len(fields) {
		return nil
	}
	return &fields[position]
}

type legacyCanonicalKeyMissingAccessForTest struct {
	fields  legacyCanonicalKeyFieldsForTest
	missing int
}

func (access legacyCanonicalKeyMissingAccessForTest) length() int {
	return access.fields.length()
}

func (access legacyCanonicalKeyMissingAccessForTest) at(position int) *legacyCanonicalKeyField {
	if position == access.missing {
		return nil
	}
	return access.fields.at(position)
}

func legacyCanonicalKeyOrderFieldsForTest(keys []string) []legacyCanonicalKeyField {
	fields := make([]legacyCanonicalKeyField, len(keys))
	for index, key := range keys {
		fields[index] = legacyCanonicalKeyField{key: key, value: index, childKeys: index*17 + 3}
	}
	return fields
}

func advanceLegacyCanonicalKeyOrderForTest(test testing.TB, order *legacyCanonicalKeyOrder, fields []legacyCanonicalKeyField, remaining int) (int, bool, bool) {
	test.Helper()
	before := *order
	comparing := before.phase == legacyCanonicalKeyOrderCompareChildren || before.phase == legacyCanonicalKeyOrderCompareParent
	var leftField, rightField legacyCanonicalKeyField
	if comparing {
		leftField, rightField = fields[before.compareLeft], fields[before.compareRight]
	}
	used, complete, yield, err := order.advance(legacyCanonicalKeyFieldsForTest(fields), remaining)
	if err != nil {
		test.Fatal("key order advance failed", err)
	}
	if used < 0 || used > remaining || used%2 != 0 {
		test.Fatalf("byte budget violated: used=%d remaining=%d", used, remaining)
	}
	if used != 0 && !comparing {
		test.Fatal("structural transition charged key bytes")
	}
	if complete != (order.phase == legacyCanonicalKeyOrderDone) || complete && yield {
		test.Fatal("completion and yield disagree with phase", complete, yield, order.phase)
	}
	if before.phase == legacyCanonicalKeyOrderDone {
		if *order != before || used != 0 || !complete || yield {
			test.Fatal("completed order changed on retry")
		}
	} else if order.work == before.work {
		if !yield || used != 0 || *order != before || !comparing || remaining >= 2 {
			test.Fatal("no-progress call was not an unchanged budget yield")
		}
	} else if order.work != before.work+1 {
		test.Fatal("advance did not perform exactly one progress action", before.work, order.work)
	}
	if yield && (!comparing || remaining >= 2 && used == 0) {
		test.Fatal("yield did not identify a blocked or partial comparison")
	}
	if comparing {
		if fields[before.compareLeft] != leftField || fields[before.compareRight] != rightField {
			test.Fatal("comparison changed its source fields")
		}
		if order.phase == before.phase && order.compareOffset != before.compareOffset+used/2 {
			test.Fatal("partial comparison lost its prefix offset")
		}
	}
	return used, complete, yield
}

func assertLegacyCanonicalKeyOrderFieldsForTest(test testing.TB, original []string, fields []legacyCanonicalKeyField) {
	test.Helper()
	expected := append([]string(nil), original...)
	sort.Strings(expected)
	seen := make([]bool, len(original))
	for index, field := range fields {
		if field.key != expected[index] {
			test.Fatalf("key order differs at %d: got=%q want=%q", index, field.key, expected[index])
		}
		identity, valid := field.value.(int)
		if !valid || identity < 0 || identity >= len(original) || seen[identity] {
			test.Fatal("field value permutation changed", index, field.value)
		}
		if field.key != original[identity] || field.childKeys != identity*17+3 {
			test.Fatal("swap separated key, value, or childKeys", index, identity, field.childKeys)
		}
		seen[identity] = true
	}
}

func drainLegacyCanonicalKeyOrderForTest(test testing.TB, keys []string, budgets []int) ([]legacyCanonicalKeyField, legacyCanonicalKeyOrder) {
	test.Helper()
	fields := legacyCanonicalKeyOrderFieldsForTest(keys)
	order := newLegacyCanonicalKeyOrder(len(fields), false)
	keyBytes := 0
	for _, key := range keys {
		keyBytes += len(key)
	}
	levels := 0
	for count := len(fields); count > 0; count /= 2 {
		levels++
	}
	stepLimit := 1024 + (len(fields)+keyBytes+1)*(levels+1)*64
	for step := 0; step < stepLimit; step++ {
		_, complete, _ := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, budgets[step%len(budgets)])
		if complete {
			assertLegacyCanonicalKeyOrderFieldsForTest(test, keys, fields)
			advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 0)
			return fields, order
		}
	}
	test.Fatal("key order did not terminate", len(fields), order.work)
	return nil, order
}

func TestLegacyCanonicalKeyOrderBudgetsAndPairing(test *testing.T) {
	ascending := make([]string, 1025)
	reverse := make([]string, len(ascending))
	for index := range ascending {
		ascending[index] = fmt.Sprintf("key-%04d", index)
		reverse[len(reverse)-1-index] = ascending[index]
	}
	prefix := strings.Repeat("한🙂", 10000)
	cases := []struct {
		name string
		keys []string
	}{
		{name: "nil"},
		{name: "empty", keys: []string{}},
		{name: "single", keys: []string{"only"}},
		{name: "single-empty", keys: []string{""}},
		{name: "reverse", keys: []string{"z", "y", "x", "c", "b", "a"}},
		{name: "duplicates", keys: []string{"b", "a", "b", "a", "a", "b", "b"}},
		{name: "empty-keys", keys: []string{"z", "", "a", "", "", "b"}},
		{name: "all-empty", keys: []string{"", "", "", "", ""}},
		{name: "prefixes", keys: []string{"abc", "a", "ab", "ab\x00", "", "abcd"}},
		{name: "utf8-and-bytes", keys: []string{"🙂", "한", "z", "é", "a", "中", "\x00", "\x7f", "\x80", "\xff", "\xe2\x82"}},
		{name: "long-common-prefix", keys: []string{prefix + "z", prefix + "a", prefix, prefix + "a"}},
		{name: "ascending-1025", keys: ascending},
		{name: "reverse-1025", keys: reverse},
	}
	for _, candidate := range cases {
		for _, budget := range []int{2, 7, 64 * 1024} {
			test.Run(fmt.Sprintf("%s/%d", candidate.name, budget), func(test *testing.T) {
				drainLegacyCanonicalKeyOrderForTest(test, candidate.keys, []int{budget})
			})
		}
		test.Run(candidate.name+"/varying", func(test *testing.T) {
			drainLegacyCanonicalKeyOrderForTest(test, candidate.keys, []int{0, 1, 2, 3, 7, 64 * 1024})
		})
	}
}

func TestLegacyCanonicalKeyOrderPagedAccessMatchesSlice(test *testing.T) {
	type orderingCase struct {
		name string
		keys []string
	}
	cases := make([]orderingCase, 0, 9)
	pool := []string{"", "a", "a\x00", "é", "한", "🙂"}
	for _, count := range []int{0, 1, 8, 9, 64, 65, 4096, 4097} {
		keys := make([]string, count)
		for index := range keys {
			keys[index] = fmt.Sprintf("%s-%04d", pool[index%len(pool)], (count-index)%257)
		}
		cases = append(cases, orderingCase{name: fmt.Sprintf("fields-%d", count), keys: keys})
	}
	prefix := strings.Repeat("한🙂", 10000)
	cases = append(cases, orderingCase{name: "long-common-prefix", keys: []string{prefix + "z", prefix + "a", prefix, prefix + "a"}})
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			fields := legacyCanonicalKeyOrderFieldsForTest(candidate.keys)
			var paged legacyCanonicalPages[legacyCanonicalKeyField]
			for _, field := range fields {
				if err := paged.append(field); err != nil {
					test.Fatal(err)
				}
			}
			order := newLegacyCanonicalKeyOrder(len(fields), false)
			pagedOrder := order
			keyBytes, levels := 0, 0
			for _, key := range candidate.keys {
				keyBytes += len(key)
			}
			for count := len(fields); count > 0; count /= 2 {
				levels++
			}
			budgets := []int{0, 1, 2, 3, 7, 64 * 1024}
			stepLimit := 1024 + (len(fields)+keyBytes+1)*(levels+1)*64
			for step := 0; step < stepLimit; step++ {
				remaining := budgets[step%len(budgets)]
				used, complete, yield := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, remaining)
				pagedUsed, pagedComplete, pagedYield, err := pagedOrder.advance(&paged, remaining)
				if err != nil || pagedUsed != used || pagedComplete != complete || pagedYield != yield || pagedOrder != order {
					test.Fatal("paged access changed ordering state, work, byte charge, or yield", step, remaining, err)
				}
				if !complete {
					continue
				}
				assertLegacyCanonicalKeyOrderFieldsForTest(test, candidate.keys, fields)
				if paged.length() != len(fields) {
					test.Fatal("ordering changed paged field count")
				}
				for index, field := range fields {
					actual := paged.at(index)
					if actual == nil || *actual != field {
						test.Fatal("paged ordering changed key, value, or childKeys association", index)
					}
				}
				before := pagedOrder
				pagedUsed, pagedComplete, pagedYield, err = pagedOrder.advance(&paged, 0)
				if err != nil || pagedUsed != 0 || !pagedComplete || pagedYield || pagedOrder != before {
					test.Fatal("completed paged order changed on retry", err)
				}
				return
			}
			test.Fatal("paged ordering did not terminate", len(fields), pagedOrder.work)
		})
	}
}

func nextLegacyCanonicalKeyPermutationForTest(permutation []int) bool {
	pivot := len(permutation) - 2
	for pivot >= 0 && permutation[pivot] >= permutation[pivot+1] {
		pivot--
	}
	if pivot < 0 {
		return false
	}
	successor := len(permutation) - 1
	for permutation[successor] <= permutation[pivot] {
		successor--
	}
	permutation[pivot], permutation[successor] = permutation[successor], permutation[pivot]
	for left, right := pivot+1, len(permutation)-1; left < right; left, right = left+1, right-1 {
		permutation[left], permutation[right] = permutation[right], permutation[left]
	}
	return true
}

func TestLegacyCanonicalKeyOrderDeterministicPermutations(test *testing.T) {
	pool := []string{"", "a", "a\x00", "aa", "é", "🙂"}
	for count := 0; count <= len(pool); count++ {
		permutation := make([]int, count)
		for index := range permutation {
			permutation[index] = index
		}
		for {
			keys := make([]string, count)
			for index, source := range permutation {
				keys[index] = pool[source]
			}
			drainLegacyCanonicalKeyOrderForTest(test, keys, []int{2})
			drainLegacyCanonicalKeyOrderForTest(test, keys, []int{64 * 1024})
			if !nextLegacyCanonicalKeyPermutationForTest(permutation) {
				break
			}
		}
	}
	for _, count := range []int{7, 16, 31, 64, 127, 1025} {
		for seed := uint64(1); seed <= 16; seed++ {
			keys := make([]string, count)
			for index := range keys {
				keys[index] = fmt.Sprintf("%s-%04d", pool[index%len(pool)], index%23)
			}
			state := seed
			for index := len(keys) - 1; index > 0; index-- {
				state = state*6364136223846793005 + 1442695040888963407
				target := int(state % uint64(index+1))
				keys[index], keys[target] = keys[target], keys[index]
			}
			drainLegacyCanonicalKeyOrderForTest(test, keys, []int{0, 1, 2, 3, 7})
			drainLegacyCanonicalKeyOrderForTest(test, keys, []int{64 * 1024})
		}
	}
}

func TestLegacyCanonicalKeyOrderFastCompletion(test *testing.T) {
	for _, count := range []int{0, 1, 1025} {
		keys := make([]string, count)
		for index := range keys {
			keys[index] = fmt.Sprintf("%04d", index)
		}
		fields := legacyCanonicalKeyOrderFieldsForTest(keys)
		order := newLegacyCanonicalKeyOrder(count, true)
		advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 0)
		advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 1)
		if order.work != 0 {
			test.Fatal("trusted sorted input did ordering work", count, order.work)
		}
		assertLegacyCanonicalKeyOrderFieldsForTest(test, keys, fields)
		if count < 2 {
			order = newLegacyCanonicalKeyOrder(count, false)
			advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 0)
			if order.work != 0 {
				test.Fatal("tiny input did ordering work", count, order.work)
			}
		}
	}
	for _, keys := range [][]string{{"", ""}, {"", "long"}, {"long", ""}, {"", "", "", ""}} {
		drainLegacyCanonicalKeyOrderForTest(test, keys, []int{0})
	}
}

func legacyCanonicalKeyOrderParentComparisonForTest(test testing.TB, keys []string) ([]legacyCanonicalKeyField, legacyCanonicalKeyOrder) {
	test.Helper()
	fields := legacyCanonicalKeyOrderFieldsForTest(keys)
	order := newLegacyCanonicalKeyOrder(len(fields), false)
	for action := 0; action < 4; action++ {
		if order.phase == legacyCanonicalKeyOrderCompareParent {
			return fields, order
		}
		_, complete, yield := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 0)
		if complete || yield {
			test.Fatal("comparison setup unexpectedly completed or blocked")
		}
	}
	test.Fatal("comparison setup did not reach parent comparison")
	return nil, order
}

func TestLegacyCanonicalKeyOrderComparisonPrefixes(test *testing.T) {
	prefix := strings.Repeat("a", 2*64*1024+3)
	fields, order := legacyCanonicalKeyOrderParentComparisonForTest(test, []string{prefix + "b", prefix + "a"})
	budgets := []int{0, 1, 2, 3, 7, 64 * 1024}
	for action := 0; action < 1024; action++ {
		before := order
		remaining := budgets[action%len(budgets)]
		width := min(remaining/2, len(prefix)+1-before.compareOffset)
		used, complete, yield := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, remaining)
		if used != 2*width || complete {
			test.Fatal("comparison read beyond its exact two-sided prefix", used, width, complete)
		}
		if before.compareOffset+width < len(prefix)+1 {
			if order.phase != before.phase || order.compareOffset != before.compareOffset+width || !yield {
				test.Fatal("common prefix was not resumed exactly", before.compareOffset, order.compareOffset, yield)
			}
			continue
		}
		if yield || order.phase != legacyCanonicalKeyOrderBuild || order.compareOffset != 0 {
			test.Fatal("last differing byte did not resolve comparison")
		}
		return
	}
	test.Fatal("long prefix comparison did not terminate")
}

func TestLegacyCanonicalKeyOrderExactComparisonCharges(test *testing.T) {
	cases := []struct {
		name      string
		keys      []string
		remaining int
		used      int
		swap      bool
	}{
		{name: "empty-equal", keys: []string{"", ""}, remaining: 0},
		{name: "empty-child", keys: []string{"a", ""}, remaining: 0},
		{name: "empty-parent", keys: []string{"", "a"}, remaining: 0, swap: true},
		{name: "first-byte-less", keys: []string{"zebra", "apple"}, remaining: 65536, used: 2},
		{name: "first-byte-greater", keys: []string{"apple", "zebra"}, remaining: 65536, used: 2, swap: true},
		{name: "third-byte", keys: []string{"abz", "aba"}, remaining: 7, used: 6},
		{name: "equal", keys: []string{"abc", "abc"}, remaining: 6, used: 6},
		{name: "short-child", keys: []string{"abcd", "abc"}, remaining: 6, used: 6},
		{name: "short-parent", keys: []string{"abc", "abcd"}, remaining: 6, used: 6, swap: true},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			fields, order := legacyCanonicalKeyOrderParentComparisonForTest(test, candidate.keys)
			used, complete, yield := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, candidate.remaining)
			if used != candidate.used || complete || yield {
				test.Fatal("comparison charge or resolution differs", used, complete, yield)
			}
			if (order.phase == legacyCanonicalKeyOrderSwap) != candidate.swap {
				test.Fatal("comparison lexicographic result differs", order.phase, candidate.swap)
			}
		})
	}
}

func TestLegacyCanonicalKeyOrderSharedCallerBudget(test *testing.T) {
	prefix := strings.Repeat("한🙂", 10000)
	keys := make([]string, 9)
	for index := range keys {
		keys[index] = prefix + fmt.Sprintf("%02d", len(keys)-index)
	}
	fields := legacyCanonicalKeyOrderFieldsForTest(keys)
	order := newLegacyCanonicalKeyOrder(len(fields), false)
	blockedWindows := 0
	for window := 0; window < 4096; window++ {
		remaining := 64 * 1024
		beforeWork := order.work
		for action := 0; action < 1024; action++ {
			before := order
			used, complete, yield := advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, remaining)
			remaining -= used
			if remaining < 0 {
				test.Fatal("ordering bypassed the caller's shared budget")
			}
			if complete {
				if blockedWindows < 2 || order.work <= beforeWork {
					test.Fatal("long ordering did not span real progress windows", blockedWindows)
				}
				assertLegacyCanonicalKeyOrderFieldsForTest(test, keys, fields)
				return
			}
			if order.work == before.work {
				if remaining != 0 || used != 0 || !yield || order != before {
					test.Fatal("exhausted shared budget restarted or advanced comparison")
				}
				blockedWindows++
				break
			}
		}
		if order.work <= beforeWork {
			test.Fatal("sorting window had no aggregate work progress")
		}
	}
	test.Fatal("shared-budget ordering did not finish")
}

func TestLegacyCanonicalKeyOrderJSONParity(test *testing.T) {
	keys := []string{"🙂", "z", "", "\x00", "a", "é", "한", "<", "\xff", "\x80", "\xe2\x82", "a\x00", "a\n"}
	fields, _ := drainLegacyCanonicalKeyOrderForTest(test, keys, []int{0, 1, 2, 3, 7})
	values := make(map[string]any, len(keys))
	for index, key := range keys {
		values[key] = index
	}
	expected, err := json.Marshal(values)
	if err != nil {
		test.Fatal(err)
	}
	actual := []byte{'{'}
	for index, field := range fields {
		if index > 0 {
			actual = append(actual, ',')
		}
		keyJSON, err := json.Marshal(field.key)
		if err != nil {
			test.Fatal(err)
		}
		valueJSON, err := json.Marshal(field.value)
		if err != nil {
			test.Fatal(err)
		}
		actual = append(actual, keyJSON...)
		actual = append(actual, ':')
		actual = append(actual, valueJSON...)
	}
	actual = append(actual, '}')
	if !bytes.Equal(actual, expected) {
		test.Fatalf("JSON map key ordering differs: got=%s want=%s", actual, expected)
	}
}

func assertLegacyCanonicalKeyOrderIntegrityForTest(test testing.TB, order *legacyCanonicalKeyOrder, fields []legacyCanonicalKeyField, remaining int) {
	test.Helper()
	var before legacyCanonicalKeyOrder
	if order != nil {
		before = *order
	}
	snapshot := append([]legacyCanonicalKeyField(nil), fields...)
	used, complete, yield, err := order.advance(legacyCanonicalKeyFieldsForTest(fields), remaining)
	if !errors.Is(err, ErrIntegrity) || used != 0 || complete || yield {
		test.Fatal("invalid ordering input did not fail cleanly", used, complete, yield, err)
	}
	if order != nil && *order != before {
		test.Fatal("integrity failure changed ordering state")
	}
	for index := range fields {
		if fields[index] != snapshot[index] {
			test.Fatal("integrity failure changed fields", index)
		}
	}
}

func TestLegacyCanonicalKeyOrderInvalidInputs(test *testing.T) {
	assertLegacyCanonicalKeyOrderIntegrityForTest(test, nil, nil, 0)
	for _, sorted := range []bool{false, true} {
		for _, count := range []int{-1, 0, 1, 2, 1025} {
			order := newLegacyCanonicalKeyOrder(count, sorted)
			fields := make([]legacyCanonicalKeyField, max(0, count))
			if count < 0 {
				assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields, 0)
				continue
			}
			assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields, -1)
			mismatch := make([]legacyCanonicalKeyField, count+1)
			assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, mismatch, 65536)
			if count > 0 {
				assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields[:count-1], 0)
			}
		}
	}
	fields, order := drainLegacyCanonicalKeyOrderForTest(test, []string{"z", "a"}, []int{2})
	assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields[:1], 65536)
	var zero legacyCanonicalKeyOrder
	assertLegacyCanonicalKeyOrderIntegrityForTest(test, &zero, nil, 0)
}

func TestLegacyCanonicalKeyOrderNilAccess(test *testing.T) {
	for _, sorted := range []bool{false, true} {
		order := newLegacyCanonicalKeyOrder(0, sorted)
		before := order
		for _, remaining := range []int{0, 1, 64 * 1024} {
			used, complete, yield, err := order.advance(nil, remaining)
			if err != nil || used != 0 || !complete || yield || order != before {
				test.Fatal("nil access changed empty ordering semantics", sorted, remaining, err)
			}
		}
		for _, count := range []int{1, 2, 1025} {
			order = newLegacyCanonicalKeyOrder(count, sorted)
			before = order
			used, complete, yield, err := order.advance(nil, 0)
			if !errors.Is(err, ErrIntegrity) || used != 0 || complete || yield || order != before {
				test.Fatal("nonempty ordering accepted nil access or changed state", count, sorted, err)
			}
			malformed := legacyCanonicalPages[legacyCanonicalKeyField]{count: count}
			used, complete, yield, err = order.advance(&malformed, 0)
			if !errors.Is(err, ErrIntegrity) || used != 0 || complete || yield || order != before {
				test.Fatal("nonempty ordering accepted missing page storage or changed state", count, sorted, err)
			}
		}
	}
}

func TestLegacyCanonicalKeyOrderMissingAccessPreservesState(test *testing.T) {
	fields, comparison := legacyCanonicalKeyOrderParentComparisonForTest(test, []string{"bbbb", "aaaa", "cccc", "dddd"})
	swap := comparison
	swap.phase = legacyCanonicalKeyOrderSwap
	swap.compareLeft, swap.compareRight, swap.compareOffset = -1, -1, 0
	extract := newLegacyCanonicalKeyOrder(len(fields), false)
	extract.phase, extract.build, extract.limit = legacyCanonicalKeyOrderExtract, 0, len(fields)-1
	cases := []struct {
		name    string
		order   legacyCanonicalKeyOrder
		missing int
	}{
		{name: "comparison", order: comparison, missing: comparison.root},
		{name: "swap", order: swap, missing: swap.root},
		{name: "extract", order: extract, missing: extract.limit - 1},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			order := candidate.order
			before := order
			snapshot := append([]legacyCanonicalKeyField(nil), fields...)
			access := legacyCanonicalKeyMissingAccessForTest{fields: legacyCanonicalKeyFieldsForTest(fields), missing: candidate.missing}
			used, complete, yield, err := order.advance(access, 64*1024)
			if !errors.Is(err, ErrIntegrity) || used != 0 || complete || yield || order != before {
				test.Fatal("missing field did not fail before changing ordering state", err)
			}
			for index, field := range fields {
				if field != snapshot[index] {
					test.Fatal("missing field changed another field", index)
				}
			}
		})
	}
}

func TestLegacyCanonicalKeyOrderInvalidState(test *testing.T) {
	fields, base := legacyCanonicalKeyOrderParentComparisonForTest(test, []string{"bbbb", "aaaa", "cccc", "dddd"})
	cases := []struct {
		name   string
		mutate func(*legacyCanonicalKeyOrder)
	}{
		{name: "phase-invalid", mutate: func(order *legacyCanonicalKeyOrder) { order.phase = legacyCanonicalKeyOrderInvalid }},
		{name: "phase-unknown", mutate: func(order *legacyCanonicalKeyOrder) { order.phase = 255 }},
		{name: "negative-count", mutate: func(order *legacyCanonicalKeyOrder) { order.count = -1 }},
		{name: "count-mismatch", mutate: func(order *legacyCanonicalKeyOrder) { order.count++ }},
		{name: "negative-build", mutate: func(order *legacyCanonicalKeyOrder) { order.build = -1 }},
		{name: "oversized-build", mutate: func(order *legacyCanonicalKeyOrder) { order.build = order.count }},
		{name: "negative-limit", mutate: func(order *legacyCanonicalKeyOrder) { order.limit = -1 }},
		{name: "oversized-limit", mutate: func(order *legacyCanonicalKeyOrder) { order.limit = order.count + 1 }},
		{name: "tiny-active-heap", mutate: func(order *legacyCanonicalKeyOrder) { order.limit = 1 }},
		{name: "build-after-extract", mutate: func(order *legacyCanonicalKeyOrder) { order.limit-- }},
		{name: "negative-root", mutate: func(order *legacyCanonicalKeyOrder) { order.root = -1 }},
		{name: "oversized-root", mutate: func(order *legacyCanonicalKeyOrder) { order.root = order.limit }},
		{name: "leaf-parent", mutate: func(order *legacyCanonicalKeyOrder) { order.root = order.limit - 1 }},
		{name: "root-before-build-cursor", mutate: func(order *legacyCanonicalKeyOrder) { order.root = 0 }},
		{name: "negative-candidate", mutate: func(order *legacyCanonicalKeyOrder) { order.candidate = -1 }},
		{name: "oversized-candidate", mutate: func(order *legacyCanonicalKeyOrder) { order.candidate = order.limit }},
		{name: "non-child-candidate", mutate: func(order *legacyCanonicalKeyOrder) { order.candidate = 0 }},
		{name: "wrong-left-index", mutate: func(order *legacyCanonicalKeyOrder) { order.compareLeft = 0 }},
		{name: "wrong-right-index", mutate: func(order *legacyCanonicalKeyOrder) { order.compareRight = 0 }},
		{name: "negative-offset", mutate: func(order *legacyCanonicalKeyOrder) { order.compareOffset = -1 }},
		{name: "oversized-offset", mutate: func(order *legacyCanonicalKeyOrder) { order.compareOffset = 5 }},
		{name: "children-without-sibling", mutate: func(order *legacyCanonicalKeyOrder) { order.phase = legacyCanonicalKeyOrderCompareChildren }},
		{name: "structural-compare-state", mutate: func(order *legacyCanonicalKeyOrder) { order.phase = legacyCanonicalKeyOrderSift }},
		{name: "work-overflow", mutate: func(order *legacyCanonicalKeyOrder) { order.work = ^uint64(0) }},
		{name: "extract-during-build", mutate: func(order *legacyCanonicalKeyOrder) {
			order.phase = legacyCanonicalKeyOrderExtract
			order.compareLeft, order.compareRight = -1, -1
		}},
		{name: "build-shortened-heap", mutate: func(order *legacyCanonicalKeyOrder) {
			order.phase = legacyCanonicalKeyOrderBuild
			order.build, order.limit = 0, 3
			order.compareLeft, order.compareRight = -1, -1
		}},
		{name: "invalid-complete-heap", mutate: func(order *legacyCanonicalKeyOrder) {
			order.phase = legacyCanonicalKeyOrderDone
			order.build, order.limit = 0, 2
			order.compareLeft, order.compareRight = -1, -1
		}},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			order := base
			candidate.mutate(&order)
			assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields, 65536)
		})
	}
	fields, order := legacyCanonicalKeyOrderParentComparisonForTest(test, []string{"abcd", "abce"})
	advanceLegacyCanonicalKeyOrderForTest(test, &order, fields, 2)
	fields[order.compareLeft].key = ""
	assertLegacyCanonicalKeyOrderIntegrityForTest(test, &order, fields, 2)
}

func TestLegacyCanonicalKeyOrderAdvanceStructure(test *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "doc_legacy_canonical_key_order.go", nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	if len(source.Imports) != 0 {
		test.Fatal("key ordering must not delegate comparisons or allocation through imports")
	}
	allowedCalls := map[string]map[string]bool{
		"valid":            {},
		"finishSift":       {},
		"finishComparison": {"finishSift": true},
		"advance":          {"valid": true, "finishSift": true, "finishComparison": true},
	}
	allowedFieldCalls := map[string]map[string]bool{
		"valid":   {"length": true, "at": true},
		"advance": {"at": true},
	}
	seen := make(map[string]bool)
	loops := 0
	for _, declaration := range source.Decls {
		if declared, valid := declaration.(*ast.GenDecl); valid {
			for _, specification := range declared.Specs {
				typed, valid := specification.(*ast.TypeSpec)
				if !valid || typed.Name.Name != "legacyCanonicalKeyOrder" {
					continue
				}
				structure, valid := typed.Type.(*ast.StructType)
				if !valid {
					test.Fatal("ordering state is not a scalar struct")
				}
				for _, field := range structure.Fields.List {
					identifier, valid := field.Type.(*ast.Ident)
					if !valid || identifier.Name != "int" && identifier.Name != "uint64" && identifier.Name != "legacyCanonicalKeyOrderPhase" {
						test.Fatal("ordering retains non-scalar state")
					}
				}
			}
		}
		function, valid := declaration.(*ast.FuncDecl)
		if !valid || function.Recv == nil {
			continue
		}
		permitted, valid := allowedCalls[function.Name.Name]
		if !valid {
			test.Fatal("unreviewed ordering method", function.Name.Name)
		}
		seen[function.Name.Name] = true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch current := node.(type) {
			case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt, *ast.RangeStmt, *ast.CompositeLit:
				test.Fatal("ordering advance path contains allocation or unbounded work", function.Name.Name)
			case *ast.UnaryExpr:
				if current.Op == token.AND {
					test.Fatal("ordering advance path takes an escaping address")
				}
			case *ast.CallExpr:
				switch called := current.Fun.(type) {
				case *ast.Ident:
					if called.Name != "len" && called.Name != "min" && called.Name != "uint64" {
						test.Fatal("ordering advance path allocates or calls an unreviewed function", called.Name)
					}
				case *ast.SelectorExpr:
					receiver, valid := called.X.(*ast.Ident)
					if !valid || !(receiver.Name == "order" && permitted[called.Sel.Name] || receiver.Name == "fields" && allowedFieldCalls[function.Name.Name][called.Sel.Name]) {
						test.Fatal("ordering advance path is recursive or calls an unreviewed method")
					}
				default:
					test.Fatal("ordering advance path contains an indirect call")
				}
			case *ast.ForStmt:
				loops++
				condition, valid := current.Cond.(*ast.BinaryExpr)
				if function.Name.Name != "advance" || !valid || condition.Op != token.LSS {
					test.Fatal("ordering loop is not a bounded byte scan")
				}
				cursor, cursorValid := condition.X.(*ast.Ident)
				bound, boundValid := condition.Y.(*ast.Ident)
				increment, incrementValid := current.Post.(*ast.IncDecStmt)
				if !cursorValid || cursor.Name != "scanned" || !boundValid || bound.Name != "width" || !incrementValid || increment.Tok != token.INC {
					test.Fatal("ordering byte loop lost its explicit prefix bound")
				}
			case *ast.BinaryExpr:
				for _, operand := range []ast.Expr{current.X, current.Y} {
					if identifier, valid := operand.(*ast.Ident); valid && (identifier.Name == "leftKey" || identifier.Name == "rightKey") {
						test.Fatal("ordering compares whole string keys")
					}
					if selected, valid := operand.(*ast.SelectorExpr); valid && selected.Sel.Name == "key" {
						test.Fatal("ordering compares whole field keys")
					}
				}
			}
			return true
		})
	}
	if len(seen) != len(allowedCalls) || loops != 1 {
		test.Fatal("ordering source does not match the reviewed allocation-free state machine", len(seen), loops)
	}
}
