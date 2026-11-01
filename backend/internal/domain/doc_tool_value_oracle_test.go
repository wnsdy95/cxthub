package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDocToolValueActualNodePrettyOracle(test *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		test.Skip("local Node oracle unavailable")
	}
	random := rand.New(rand.NewSource(72164))
	var generate func(int) string
	generate = func(depth int) string {
		kind := random.Intn(7)
		if depth == 0 {
			kind %= 5
		}
		switch kind {
		case 0:
			return "null"
		case 1:
			return []string{"true", "false"}[random.Intn(2)]
		case 2:
			return []string{"-0", "1e309", "1e-99999", "9007199254740993", "0.0000001", "1000000000000000100"}[random.Intn(6)]
		case 3, 4:
			return []string{`"\ud800"`, `"\udc00"`, `"\uD800\uDC00"`, `"line\n한🙂\t"`, `"quote\"slash\\"`, `""`}[random.Intn(6)]
		case 5:
			values := make([]string, random.Intn(5))
			for index := range values {
				values[index] = generate(depth - 1)
			}
			return "[" + strings.Join(values, ", \n") + "]"
		default:
			keys := []string{`"2"`, `"10"`, `"a"`, `"b"`, `"__proto__"`, `"\ud800"`, `"\udc00"`, `"\u0032"`}
			values := make([]string, random.Intn(7))
			for index := range values {
				values[index] = keys[random.Intn(len(keys))] + " \t : " + generate(depth-1)
			}
			return "{" + strings.Join(values, ", \n") + "}"
		}
	}
	tokens := []string{`{"b":{"old":1},"2":"x","\u0032":[true,{},[]],"b":{"new":2}}`, `1.` + strings.Repeat("0", 150000) + `1`, `1e-` + strings.Repeat("0", 150000) + `9999`, `{"key"` + strings.Repeat(" ", 70000) + `: "value"}`}
	for range 100 {
		tokens = append(tokens, generate(4))
	}
	input, err := json.Marshal(tokens)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", `let raw='';process.stdin.setEncoding('utf8');process.stdin.on('data',chunk=>raw+=chunk);process.stdin.on('end',()=>process.stdout.write(JSON.stringify(JSON.parse(raw).map(value=>JSON.stringify(JSON.parse(value),null,2)))));`)
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		test.Fatal("actual independent Node formatter failed", err)
	}
	var expected []string
	if json.Unmarshal(output, &expected) != nil || len(expected) != len(tokens) {
		test.Fatal("invalid oracle response")
	}
	for index, token := range tokens {
		fixture := newDocToolValueFixture(test, token)
		plan := fixture.compile(test)
		actual, _ := readDocToolValueTestPages(test, fixture, plan)
		if actual != expected[index] {
			test.Fatalf("actual Node disagreement at fixture %d\nraw=%s\ngot=%q\nwant=%q", index, token, actual, expected[index])
		}
	}
	test.Log("independent actual Node fixtures:", len(tokens))
}
