package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func prefixFixture(t testing.TB, texts ...string) (SessionDoc, VerifiedSessionDoc) {
	t.Helper()
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceProvider: ProviderCodex, SessionOriginID: "native-prefix"}}
	for i, text := range texts {
		cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Seq: i, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := SessionDoc{Hash: HashContent(raw), CIR: cir}
	v, err := VerifySessionDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, v
}

func TestCaptureSupersedesPreservesPrefixIdentityAndIntegrity(t *testing.T) {
	_, old := prefixFixture(t, "first {\"x\": [1,2]}", "second\nline")
	for _, tc := range []struct {
		name  string
		texts []string
		want  bool
	}{
		{"equal", []string{"first {\"x\": [1,2]}", "second\nline"}, true},
		{"append", []string{"first {\"x\": [1,2]}", "second\nline", "last"}, true},
		{"shorter", []string{"first {\"x\": [1,2]}"}, false},
		{"divergent", []string{"first {\"x\": [1,2]}", "different", "last"}, false},
		{"changed event", []string{"first {\"x\": [1,2]}", "second\nline extended"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, next := prefixFixture(t, tc.texts...)
			doc.CIR.Envelope.GitBranch = "another-branch"
			doc.CIR.Envelope.Cwd = "/other-worktree"
			raw, _ := CanonicalBytes(doc.CIR)
			doc.Hash = HashContent(raw)
			next, _ = VerifySessionDoc(doc)
			for _, legacy := range []bool{false, true} {
				nextRaw := next.Bytes()
				if legacy {
					nextRaw, _ = json.MarshalIndent(doc.CIR, "", "  ")
				}
				got, err := CaptureSupersedes(context.Background(), old.Reference(), old.Bytes(), next.Reference(), nextRaw, ProviderCodex, "native-prefix")
				if err != nil || got != tc.want {
					t.Fatalf("legacy=%v got=%v err=%v", legacy, got, err)
				}
			}
		})
	}
	for _, tc := range []struct {
		provider ProviderKind
		session  string
	}{{ProviderClaude, "native-prefix"}, {ProviderCodex, "another"}, {ProviderCodex, ""}, {"", "native-prefix"}} {
		got, err := CaptureSupersedes(context.Background(), old.Reference(), old.Bytes(), old.Reference(), old.Bytes(), tc.provider, tc.session)
		if err != nil || got {
			t.Fatal("unproven identity", got, err)
		}
	}
	for _, raw := range [][]byte{[]byte(`{"envelope":{},"events":[]}`), []byte(`not JSON`), []byte(strings.Replace(string(old.Bytes()), "second", "tamper", 1))} {
		if got, err := CaptureSupersedes(context.Background(), old.Reference(), old.Bytes(), old.Reference(), raw, ProviderCodex, "native-prefix"); got || !errors.Is(err, ErrIntegrity) {
			t.Fatal("forged successor", got, err)
		}
	}
	if got, err := CaptureSupersedes(context.Background(), VerifiedDocReference{}, old.Bytes(), old.Reference(), old.Bytes(), ProviderCodex, "native-prefix"); got || !errors.Is(err, ErrIntegrity) {
		t.Fatal("forged proof", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureSupersedes(ctx, old.Reference(), old.Bytes(), old.Reference(), old.Bytes(), ProviderCodex, "native-prefix"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func FuzzCapturePrefixMatchesTypedEvents(f *testing.F) {
	f.Add("one", "two", true)
	f.Add("{\"escaped\": \"value\\\"\"}", "\n\uD55C\uAD6D\uC5B4", false)
	f.Fuzz(func(t *testing.T, left, right string, appendOnly bool) {
		if len(left)+len(right) > 1<<16 {
			t.Skip()
		}
		oldDoc, old := prefixFixture(t, left)
		texts := []string{right}
		if appendOnly {
			texts = []string{left, right}
		}
		newDoc, next := prefixFixture(t, texts...)
		// Use decoded canonical values, matching stored GetDoc semantics for UTF-8.
		_ = json.Unmarshal(old.Bytes(), &oldDoc.CIR)
		_ = json.Unmarshal(next.Bytes(), &newDoc.CIR)
		want := len(newDoc.CIR.Events) >= len(oldDoc.CIR.Events) && reflect.DeepEqual(oldDoc.CIR.Events, newDoc.CIR.Events[:len(oldDoc.CIR.Events)])
		got, err := CaptureSupersedes(context.Background(), old.Reference(), old.Bytes(), next.Reference(), next.Bytes(), ProviderCodex, "native-prefix")
		if err != nil || got != want {
			t.Fatalf("got %v want %v err %v", got, want, err)
		}
	})
}

func BenchmarkCapturePrefix(b *testing.B) {
	texts := make([]string, 5000)
	for i := range texts {
		texts[i] = fmt.Sprintf("%d %s", i, strings.Repeat("synthetic capture ", 200))
	}
	oldDoc, old := prefixFixture(b, texts...)
	newDoc, next := prefixFixture(b, append(texts, "new work")...)
	oldRaw, nextRaw := old.Bytes(), next.Bytes()
	b.Run("typed", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(oldRaw) + len(nextRaw)))
		for i := 0; i < b.N; i++ {
			var a, c CIRDocument
			if json.Unmarshal(oldRaw, &a) != nil || json.Unmarshal(nextRaw, &c) != nil || !reflect.DeepEqual(a.Events, c.Events[:len(a.Events)]) {
				b.Fatal("comparison failed")
			}
		}
	})
	b.Run("verified bytes", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(oldRaw) + len(nextRaw)))
		for i := 0; i < b.N; i++ {
			got, err := CaptureSupersedes(context.Background(), old.Reference(), oldRaw, next.Reference(), nextRaw, oldDoc.CIR.Envelope.SourceProvider, newDoc.CIR.Envelope.SessionOriginID)
			if err != nil || !got {
				b.Fatal(got, err)
			}
		}
	})
}
