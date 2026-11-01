package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type legacyNativeFloat64DifferentialCounts struct {
	fast     int
	decimal  int
	accepted int
	rejected int
}

func (counts *legacyNativeFloat64DifferentialCounts) check(test *testing.T, name, raw string, from int) {
	test.Helper()
	if ConversationManifestChunkBytes != 512<<10 || canonicalCheckBytes != 64<<10 {
		test.Fatal("native differential requires the original 512 KiB parts and 64 KiB scan quantum")
	}
	if from < 1 || !json.Valid([]byte(raw)) {
		test.Fatalf("invalid generated numeric fixture %s: %.96s (length %d, from %d)", name, raw, len(raw), from)
	}
	var native any
	oracleErr := json.Unmarshal([]byte(raw), &native)
	wantRejected := oracleErr != nil
	wantBits := uint64(0)
	if wantRejected {
		var numericErr *json.UnmarshalTypeError
		if !errors.As(oracleErr, &numericErr) || numericErr.Type != reflect.TypeFor[float64]() || numericErr.Value != "number "+raw {
			test.Fatalf("native oracle did not report numeric rejection for %s: %v", name, oracleErr)
		}
	} else {
		number, ok := native.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			test.Fatalf("native oracle did not return a finite float64 for %s: %T", name, native)
		}
		wantBits = math.Float64bits(number)
	}
	buffer := &legacySourceProofBuffer{body: []byte("[" + strings.Repeat(" ", from-1) + raw + ",true]")}
	source := buffer.input()
	var activeContext context.Context
	readSource := source.Read
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if ctx != activeContext || ctx.Err() != nil {
			test.Fatalf("numeric read did not use the current live call context for %s", name)
		}
		return readSource(ctx, offset, length)
	}
	parser, err := newLegacyNativeFloat64(from, from+len(raw))
	if err != nil || parser == nil || len(buffer.reads) != 0 {
		test.Fatalf("numeric constructor failed or read source for %s: %v, reads %d", name, err, len(buffer.reads))
	}
	if parser.position != from {
		test.Fatalf("numeric constructor lost raw source offset for %s: %d, want %d", name, parser.position, from)
	}
	scanLimit := 64 + 32*len(source.Parts)
	scanSteps, conversionSteps := 0, 0
	usedDecimal := false
	for stepIndex := 0; stepIndex < scanLimit+512; stepIndex++ {
		beforePosition, beforeReads := parser.position, len(buffer.reads)
		if beforePosition < from+len(raw) {
			scanSteps++
		} else {
			conversionSteps++
		}
		if scanSteps > scanLimit || conversionSteps > 512 {
			test.Fatalf("numeric differential exceeded its finite scan/conversion allowance for %s: %d/%d", name, scanSteps, conversionSteps)
		}
		ctx, cancel := context.WithCancel(context.Background())
		activeContext = ctx
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		bits, complete, rejected, stepErr := parser.step(ctx, input)
		cancel()
		activeContext = nil
		usedDecimal = usedDecimal || parser.decimal != nil
		if stepErr != nil {
			test.Fatalf("numeric differential failed for %s at raw offset %d: %v", name, beforePosition, stepErr)
		}
		if parser.position < beforePosition || parser.position > from+len(raw) || parser.position-beforePosition > canonicalCheckBytes {
			test.Fatalf("numeric differential escaped its span/scan quantum for %s: %d -> %d", name, beforePosition, parser.position)
		}
		reads := buffer.reads[beforeReads:]
		if input.unbounded || input.loads < 0 || input.loads > 2 || input.loads != len(reads) || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes {
			test.Fatalf("numeric differential escaped shared budgets for %s: unbounded=%t loads=%d reads=%d work=%d", name, input.unbounded, input.loads, len(reads), input.workBytes)
		}
		for _, read := range reads {
			if read[0] < 0 || read[0] >= source.Bytes || read[0]%ConversationManifestChunkBytes != 0 || read[1] != min(ConversationManifestChunkBytes, source.Bytes-read[0]) {
				test.Fatalf("numeric differential bypassed authenticated full-part reads for %s: %v", name, read)
			}
		}
		if (!complete || rejected) && bits != 0 || !complete && rejected {
			test.Fatalf("numeric differential exposed pending/rejected bits for %s: %016x complete=%t rejected=%t", name, bits, complete, rejected)
		}
		var estimate docRangeHeaderHeapEstimate
		parser.retained(&estimate)
		if estimate.err != nil || estimate.bytes <= 0 || estimate.bytes >= canonicalCheckBytes {
			test.Fatalf("numeric differential lost its fixed-state retained estimate for %s: %d, %v", name, estimate.bytes, estimate.err)
		}
		for _, page := range input.pages {
			clear(page)
		}
		clear(input.window)
		if !complete {
			continue
		}
		if parser.position != from+len(raw) || rejected != wantRejected || bits != wantBits {
			test.Fatalf("native mismatch for %s: %.96s (length %d): bits=%016x rejected=%t position=%d, want bits=%016x rejected=%t position=%d", name, raw, len(raw), bits, rejected, parser.position, wantBits, wantRejected, from+len(raw))
		}
		if usedDecimal {
			counts.decimal++
		} else {
			counts.fast++
		}
		if rejected {
			counts.rejected++
		} else {
			counts.accepted++
		}
		return
	}
	test.Fatalf("numeric differential exhausted finite progress for %s", name)
}

func (counts legacyNativeFloat64DifferentialCounts) report(test *testing.T) {
	test.Helper()
	test.Logf("native differential outcomes: fast=%d decimal=%d accepted=%d rejected=%d", counts.fast, counts.decimal, counts.accepted, counts.rejected)
}

func legacyNativeFloat64DifferentialDigits(random *rand.Rand, length int, nonzero bool) string {
	digits := make([]byte, length)
	for index := range digits {
		digits[index] = byte('0' + random.Intn(10))
	}
	if nonzero {
		digits[0] = byte('1' + random.Intn(9))
	}
	return string(digits)
}

func legacyNativeFloat64DifferentialGrammar(random *rand.Rand, length int) string {
	sign := ""
	if length > 1 && random.Intn(2) == 0 {
		sign = "-"
	}
	remaining := length - len(sign)
	family := length % 8
	suffix := ""
	switch family {
	case 3:
		suffix = "e" + strconv.Itoa(random.Intn(901)-450)
	case 4:
		suffix = "E+" + strings.Repeat("0", random.Intn(7)) + strconv.Itoa(random.Intn(401))
	case 5:
		prefixes := [...]string{"9999", "10000", "10001", "99999", "100000", "999999"}
		exponentSign := [...]string{"", "+", "-"}
		suffix = "e" + exponentSign[random.Intn(len(exponentSign))] + strings.Repeat("0", random.Intn(5)) + prefixes[random.Intn(len(prefixes))] + legacyNativeFloat64DifferentialDigits(random, random.Intn(9), false)
	case 6:
		if remaining >= 4 {
			exponentDigit := "0"
			if random.Intn(2) == 0 {
				exponentDigit = "9"
			}
			return sign + "0E-" + strings.Repeat(exponentDigit, remaining-3)
		}
	case 7:
		suffix = "e-" + strconv.Itoa(max(0, length+random.Intn(681)-340))
	}
	if len(suffix) >= remaining {
		suffix = ""
	}
	remaining -= len(suffix)
	if family == 2 && remaining >= 3 {
		return sign + "0." + legacyNativeFloat64DifferentialDigits(random, remaining-2, false) + suffix
	}
	if remaining >= 3 && (family == 1 || family == 4 || family == 5 && random.Intn(2) == 0) {
		digits := legacyNativeFloat64DifferentialDigits(random, remaining-1, true)
		point := 1 + random.Intn(len(digits)-1)
		return sign + digits[:point] + "." + digits[point:] + suffix
	}
	return sign + legacyNativeFloat64DifferentialDigits(random, remaining, true) + suffix
}

func TestLegacyNativeFloat64DifferentialGeneratedGrammar(test *testing.T) {
	random := rand.New(rand.NewSource(2026102507))
	var counts legacyNativeFloat64DifferentialCounts
	dotted, undotted, exponentiated, negative := 0, 0, 0, 0
	for length := 1; length <= 2000; length++ {
		raw := legacyNativeFloat64DifferentialGrammar(random, length)
		if len(raw) != length {
			test.Fatalf("generated grammar length %d, want %d", len(raw), length)
		}
		if strings.Contains(raw, ".") {
			dotted++
		} else {
			undotted++
		}
		if strings.ContainsAny(raw, "eE") {
			exponentiated++
		}
		if strings.HasPrefix(raw, "-") {
			negative++
		}
		counts.check(test, fmt.Sprintf("grammar-%04d", length), raw, 1)
	}
	counts.report(test)
	if counts.fast == 0 || counts.decimal == 0 || counts.accepted == 0 || counts.rejected == 0 || dotted == 0 || undotted == 0 || exponentiated == 0 || negative == 0 {
		test.Fatal("generated grammar did not exercise both actual conversion paths, outcomes, and numeric spellings", counts, dotted, undotted, exponentiated, negative)
	}
}

func legacyNativeFloat64DifferentialMidpoint(lowerBits uint64) *big.Rat {
	lower := new(big.Rat).SetFloat64(math.Float64frombits(lowerBits))
	upper := new(big.Rat)
	if lowerBits == 0x7fefffffffffffff {
		upper.SetInt(new(big.Int).Lsh(big.NewInt(1), 1024))
	} else {
		upper.SetFloat64(math.Float64frombits(lowerBits + 1))
	}
	return new(big.Rat).Quo(new(big.Rat).Add(lower, upper), big.NewRat(2, 1))
}

func legacyNativeFloat64DifferentialNeighbors(midpoint *big.Rat, tail int) [3]string {
	places := midpoint.Denom().BitLen() + tail
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	unit := new(big.Rat).SetFrac(big.NewInt(1), denominator)
	return [3]string{
		new(big.Rat).Sub(midpoint, unit).FloatString(places),
		midpoint.FloatString(places),
		new(big.Rat).Add(midpoint, unit).FloatString(places),
	}
}

func TestLegacyNativeFloat64DifferentialGeneratedMidpoints(test *testing.T) {
	boundaries := []uint64{
		0, 1, 2, 0x000ffffffffffffe, 0x000fffffffffffff, 0x0010000000000000,
		0x3fefffffffffffff, 0x3ff0000000000000, 0x3ff0000000000001,
		0x7feffffffffffffe, 0x7fefffffffffffff,
	}
	random := rand.New(rand.NewSource(2026102508))
	for len(boundaries) < 48 {
		boundaries = append(boundaries, random.Uint64()%0x7ff0000000000000)
	}
	var counts legacyNativeFloat64DifferentialCounts
	for boundaryIndex, lowerBits := range boundaries {
		midpoint := legacyNativeFloat64DifferentialMidpoint(lowerBits)
		for _, tail := range []int{0, 801} {
			for neighborIndex, raw := range legacyNativeFloat64DifferentialNeighbors(midpoint, tail) {
				for _, sign := range []string{"", "-"} {
					token := sign + raw
					if len(token) > 2000 {
						test.Fatalf("generated midpoint exceeded finite small-token limit: %d", len(token))
					}
					counts.check(test, fmt.Sprintf("midpoint-%02d-%016x-tail-%d-neighbor-%d-sign-%s", boundaryIndex, lowerBits, tail, neighborIndex, sign), token, 1)
				}
			}
		}
		if boundaryIndex >= 8 {
			continue
		}
		places := midpoint.Denom().BitLen() - 1
		coefficient := strings.TrimLeft(strings.ReplaceAll(midpoint.FloatString(places), ".", ""), "0")
		for _, digits := range []int{799, 800, 801} {
			padding := digits - len(coefficient)
			if padding < 0 {
				continue
			}
			integer := coefficient + strings.Repeat("0", padding)
			exponent := "e-" + strconv.Itoa(places+padding)
			for _, fraction := range []string{"", ".0", ".1"} {
				counts.check(test, fmt.Sprintf("coefficient-%02d-digits-%d-fraction-%s", boundaryIndex, digits, fraction), integer+fraction+exponent, 1)
			}
		}
	}
	counts.report(test)
	if counts.decimal == 0 || counts.accepted == 0 || counts.rejected == 0 {
		test.Fatal("generated midpoints omitted decimal conversion or native acceptance/rejection", counts)
	}
}

func TestLegacyNativeFloat64DifferentialScanBoundaries(test *testing.T) {
	midpoint := legacyNativeFloat64DifferentialMidpoint(0x3ff0000000000013)
	places := midpoint.Denom().BitLen() - 1
	exact := midpoint.FloatString(places)
	var counts legacyNativeFloat64DifferentialCounts
	for boundaryIndex, length := range []int{canonicalCheckBytes - 1, canonicalCheckBytes, canonicalCheckBytes + 1} {
		last := "0"
		if boundaryIndex == 2 {
			last = "1"
		}
		raw := exact + strings.Repeat("0", length-len(exact)-1) + last
		from := [...]int{1, ConversationManifestChunkBytes - 19, ConversationManifestChunkBytes - 800}
		counts.check(test, fmt.Sprintf("sticky-scan-length-%d", length), raw, from[boundaryIndex])
	}
	counts.check(test, "exponent-sign-on-part-boundary", "7e+"+strings.Repeat("0", canonicalCheckBytes+1)+"2", ConversationManifestChunkBytes-2)
	counts.check(test, "zero-exponent-digits-on-part-boundary", "-0e-"+strings.Repeat("9", canonicalCheckBytes+1), ConversationManifestChunkBytes-4)
	coefficient := strings.ReplaceAll(exact, ".", "")
	padding := canonicalCheckBytes + 1 - len(coefficient)
	integer := coefficient + strings.Repeat("0", padding)
	exponent := "e-" + strconv.Itoa(places+padding)
	counts.check(test, "compensated-integer-after-scan-boundary", integer+exponent, 1)
	counts.check(test, "compensated-dot-after-scan-boundary", integer+".1"+exponent, ConversationManifestChunkBytes-800)
	counts.check(test, "source-prefix-freeze-compensation", "0."+strings.Repeat("0", 100000)+"7e1000017", 1)
	counts.report(test)
	if counts.fast == 0 || counts.decimal == 0 {
		test.Fatal("selected scan boundaries omitted an actual fast or decimal path", counts)
	}
}
