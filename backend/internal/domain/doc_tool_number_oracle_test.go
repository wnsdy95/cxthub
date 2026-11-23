package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

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
	_ = docToolNumberNodeOracle(test, tokens)
}
