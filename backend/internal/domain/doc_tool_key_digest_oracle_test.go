package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

func TestDocToolKeyDigestMatchesLocalJavaScript(test *testing.T) {
	tokens := []string{
		`""`, `"0"`, `"00"`, `"01"`, `"-0"`, `"2"`, `"10"`, `"4294967294"`, `"4294967295"`,
		`"\u0030"`, `"\u0032"`, `"\u0030\u0031"`, `"ordinary"`, `"\u006frdinary"`,
		`"\uD800"`, `"\ud800"`, `"\ud801"`, `"\udc00"`, `"\ufffd"`,
		`"\ud83d\ude42"`, `"🙂"`, `"\u0000<&>\u2028\u2029\"\\"`,
	}
	node, err := exec.LookPath("node")
	if err != nil {
		test.Skip("optional local JS digest oracle unavailable; frozen key tests remain active")
	}
	input, err := json.Marshal(tokens)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", `const fs=require('node:fs'), crypto=require('node:crypto'); const tokens=JSON.parse(fs.readFileSync(0,'utf8')); process.stdout.write(JSON.stringify(tokens.map(token=>{const key=JSON.parse(token), index=key>>>0; return {digest:'sha256:'+crypto.createHash('sha256').update(JSON.stringify(key),'utf8').digest('hex'), numeric:key===String(index)&&index!==4294967295, index};})));`)
	command.Stdin = bytes.NewReader(input)
	raw, err := command.Output()
	if err != nil {
		test.Fatal(err)
	}
	var expected []struct {
		Digest  ContentHash `json:"digest"`
		Numeric bool        `json:"numeric"`
		Index   uint32      `json:"index"`
	}
	if json.Unmarshal(raw, &expected) != nil || len(expected) != len(tokens) {
		test.Fatal("bad JS oracle response")
	}
	for index, token := range tokens {
		fixture := prepareDocToolStringFixture(test, token, DocToolStringQuoted)
		source, err := NewDocToolKeyDigestSource(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		state, err := NewDocToolKeyDigestState(source)
		if err != nil {
			test.Fatal(err)
		}
		load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
		for !state.Done {
			step, err := StepDocToolKeyDigest(context.Background(), source, state, load)
			if err != nil {
				test.Fatal(err)
			}
			state = step.State
		}
		result, err := FinishDocToolKeyDigest(context.Background(), source, state, load)
		want := expected[index]
		if err != nil || result.Digest != want.Digest || result.IsArrayIndex != want.Numeric || want.Numeric && result.ArrayIndex != want.Index {
			test.Fatal("semantic key digest/index differs from actual JS", token, result, want, err)
		}
	}
}
