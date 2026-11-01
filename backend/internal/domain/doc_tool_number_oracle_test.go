package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func docToolNumberExactDecimal(value *big.Rat) string {
	places := value.Denom().BitLen() - 1
	coefficient := new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(places)), nil)
	coefficient.Mul(coefficient, value.Num())
	digits := coefficient.String()
	if places == 0 {
		return digits
	}
	if places >= len(digits) {
		digits = strings.Repeat("0", places-len(digits)+1) + digits
	}
	point := len(digits) - places
	return strings.TrimRight(strings.TrimRight(digits[:point]+"."+digits[point:], "0"), ".")
}

func docToolNumberBoundaryNeighbors(exact string, tail int) []string {
	if !strings.Contains(exact, ".") {
		exact += "."
	}
	point := strings.IndexByte(exact, '.')
	integer := strings.ReplaceAll(exact, ".", "") + strings.Repeat("0", tail) + "0"
	below, above := []byte(integer), []byte(integer)
	above[len(above)-1] = '1'
	for index := len(below) - 1; index >= 0; index-- {
		if below[index] != '0' {
			below[index]--
			break
		}
		below[index] = '9'
	}
	insert := func(digits []byte) string {
		whole := strings.TrimLeft(string(digits[:point]), "0")
		if whole == "" {
			whole = "0"
		}
		return whole + "." + string(digits[point:])
	}
	return []string{insert(below), insert([]byte(integer)), insert(above)}
}

func docToolNumberMidpoint(bits uint64) *big.Rat {
	lower := new(big.Rat).SetFloat64(math.Float64frombits(bits))
	var upper *big.Rat
	if bits == 0x7fefffffffffffff {
		upper = new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 1024))
	} else {
		upper = new(big.Rat).SetFloat64(math.Float64frombits(bits + 1))
	}
	return new(big.Rat).Quo(new(big.Rat).Add(lower, upper), big.NewRat(2, 1))
}

func docToolNumberNodeOracle(test *testing.T, tokens []string) []string {
	test.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		test.Skip("local Node oracle unavailable; deterministic formatting and exact boundary proof remain active")
	}
	input, err := json.Marshal(tokens)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", `const fs=require('node:fs'); const tokens=JSON.parse(fs.readFileSync(0,'utf8')); process.stdout.write(JSON.stringify(tokens.map(token=>{const value=JSON.parse(token); if (typeof value!=='number') throw Error('not numeric'); const text=JSON.stringify(value); if (text!==JSON.stringify(Number(token))) throw Error('different number conversion'); return text;})));`)
	command.Stdin = bytes.NewReader(input)
	raw, err := command.Output()
	if err != nil {
		test.Fatal("local JS oracle failed", err)
	}
	var expected []string
	if json.Unmarshal(raw, &expected) != nil || len(expected) != len(tokens) {
		test.Fatal("bad JS oracle response")
	}
	return expected
}

func TestDocToolNumberRetentionBoundaryProof(test *testing.T) {
	const proof = `Every positive binary64 nearest-rounding boundary, including zero/subnormal and finite/overflow boundaries, is n*2^q with 0<n<2^54 and -1075<=q<=970. For q<0 its terminating decimal coefficient is at most (2^54-1)*5^1075<10^768; for q>=0 it is below 2^1024<10^309. These integer inequalities are checked exactly below. Removing decimal trailing zeros cannot increase its significant-digit count. Thus every boundary has at most 768 significant decimal digits, strictly fewer than the retained 2048. After 2048 significant digits P, any omitted suffix lies in [P,P+1)*10^q. A rounding boundary cannot lie strictly inside this decimal grid cell: its shorter terminating coefficient puts it on the grid. A zero suffix selects the lower endpoint exactly; a nonzero suffix and the representative (10P+1)*10^(q-1) are both strictly inside the same cell, on the same side of every rounding boundary. Parsing the at-most-2049-digit representative therefore preserves nearest-even rounding, including exact ties, underflow and overflow. The representative is normalized with the decimal point after its first digit, so the decimal-point position is not confused with a parser's retained digit count; the local Go parser's 800 digits plus truncation sticky exceed the exact 768-digit boundary bound. If the explicit exponent saturates at tokenLength+4096, all remaining decimal-point/significand corrections have magnitude at most tokenLength, and the retained coefficient has at most 2049 digits, so positive saturation is certainly overflow and negative saturation is certainly underflow. Counters are exact and at most the bounded token length; only the exponent saturates.`
	test.Log(proof)
	if DocToolNumberRetainedDigits <= 1129 {
		test.Fatal("retention no longer exceeds the proven boundary bound")
	}
	numerator := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 54), big.NewInt(1))
	negativeBound := new(big.Int).Mul(numerator, new(big.Int).Exp(big.NewInt(5), big.NewInt(1075), nil))
	if negativeBound.Cmp(new(big.Int).Exp(big.NewInt(10), big.NewInt(768), nil)) >= 0 || new(big.Int).Lsh(big.NewInt(1), 1024).Cmp(new(big.Int).Exp(big.NewInt(10), big.NewInt(309), nil)) >= 0 {
		test.Fatal("exact decimal boundary inequalities failed")
	}
	maxDigits := 0
	for exponent := -1075; exponent <= 970; exponent++ {
		coefficient := new(big.Int).Set(numerator)
		if exponent < 0 {
			coefficient.Mul(coefficient, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(-exponent)), nil))
		} else {
			coefficient.Lsh(coefficient, uint(exponent))
		}
		digits := len(coefficient.String())
		maxDigits = max(maxDigits, digits)
		if digits > 768 || digits >= DocToolNumberRetainedDigits {
			test.Fatal("binary64 boundary outside proven digit bound", exponent, digits)
		}
	}
	maximum := new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(DocToolNumberRetainedDigits), nil), big.NewInt(1))
	if maximum.BitLen() > docToolNumberCoefficientLimbs*64 {
		test.Fatal("fixed binary coefficient cannot hold the retained digits")
	}
	test.Log("largest coefficient in exhaustive exponent envelope:", maxDigits, "digits; fixed coefficient:", maximum.BitLen(), "bits")
}

func TestDocToolNumberActualNodeOrdinaryAndRandom(test *testing.T) {
	tokens := []string{
		"0", "-0", "1", "-1", "1.25", "1E+00000009", "1e-6", "1e-7", "1e20", "1e21", "1e22", "1e23",
		"-1e999999999999999999999", "-0e99999999999999999999", "1e-99999999999999999999", "2.2250738585072014e-308",
		"9007199254740993", "1000000000000000128", "0.0000009999999999999999", "999999999999999999999",
	}
	random := rand.New(rand.NewSource(20261021))
	for index := 0; index < 1500; index++ {
		bits := random.Uint64()
		if bits&0x7ff0000000000000 == 0x7ff0000000000000 {
			bits ^= 0x0010000000000000
		}
		value := math.Float64frombits(bits)
		tokens = append(tokens, strconv.FormatFloat(value, 'e', 25, 64))
	}
	for index := 0; index < 500; index++ {
		count := random.Intn(4200) + 1
		digits := make([]byte, count)
		digits[0] = byte('1' + random.Intn(9))
		for position := 1; position < len(digits); position++ {
			digits[position] = byte('0' + random.Intn(10))
		}
		point := random.Intn(count) + 1
		token := string(digits[:point])
		if point < count {
			token += "." + string(digits[point:])
		}
		if random.Intn(2) == 0 {
			token = "-" + token
		}
		tokens = append(tokens, token+"e"+strconv.Itoa(random.Intn(1400)-700-point))
	}
	expected := docToolNumberNodeOracle(test, tokens)
	for index, token := range tokens {
		plan, _ := docToolNumberTestPrepare(test, token)
		if plan.Text != expected[index] {
			test.Fatalf("Node mismatch at %d: %.80s (length %d): got %s, want %s", index, token, len(token), plan.Text, expected[index])
		}
	}
}

func TestDocToolNumberActualNodeMidpointsAndDistantSticky(test *testing.T) {
	boundaries := []uint64{
		0, 1, 2, 3, 0x000ffffffffffffe, 0x000fffffffffffff, 0x0010000000000000, 0x0010000000000001,
		math.Float64bits(1) - 1, math.Float64bits(1), math.Float64bits(1) + 1,
		math.Float64bits(1e-6) - 1, math.Float64bits(1e-6), math.Float64bits(1e-6) + 1,
		math.Float64bits(1e21) - 1, math.Float64bits(1e21), math.Float64bits(1e21) + 1,
		0x7feffffffffffffe, 0x7fefffffffffffff,
	}
	random := rand.New(rand.NewSource(42))
	for index := 0; index < 256; index++ {
		boundaries = append(boundaries, random.Uint64()%0x7ff0000000000000)
	}
	var tokens []string
	for _, bits := range boundaries {
		exact := docToolNumberExactDecimal(docToolNumberMidpoint(bits))
		for _, token := range docToolNumberBoundaryNeighbors(exact, DocToolNumberRetainedDigits+37) {
			tokens = append(tokens, token, "-"+token)
		}
	}
	for _, bits := range []uint64{0, 1, 0x000fffffffffffff, math.Float64bits(1), 0x7fefffffffffffff} {
		exact := docToolNumberExactDecimal(docToolNumberMidpoint(bits))
		for _, token := range docToolNumberBoundaryNeighbors(exact, 140003) {
			tokens = append(tokens, token, "-"+token)
		}
	}
	expected := docToolNumberNodeOracle(test, tokens)
	for index, token := range tokens {
		plan, _ := docToolNumberTestPrepare(test, token)
		if plan.Text != expected[index] {
			test.Fatalf("Node midpoint mismatch at %d (length %d): got %s, want %s", index, len(token), plan.Text, expected[index])
		}
	}
	for _, bits := range []uint64{0, 1, math.Float64bits(1), math.Float64bits(1) + 1} {
		exact := docToolNumberExactDecimal(docToolNumberMidpoint(bits))
		neighbors := docToolNumberBoundaryNeighbors(exact, 140003)
		below, _ := docToolNumberTestPrepare(test, neighbors[0])
		tie, _ := docToolNumberTestPrepare(test, neighbors[1])
		above, _ := docToolNumberTestPrepare(test, neighbors[2])
		if below.Text == above.Text || bits%2 == 0 && tie.Text != below.Text || bits%2 == 1 && tie.Text != above.Text {
			test.Fatal("distant nonzero lost tie-to-even evidence", fmt.Sprintf("%x", bits), below.Text, tie.Text, above.Text)
		}
	}
}
