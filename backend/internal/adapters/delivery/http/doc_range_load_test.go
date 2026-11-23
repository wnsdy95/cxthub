//go:build postgres

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rangeLoadEvent struct {
	Hash  domain.ContentHash
	Bytes int
	Kind  domain.EventKind
	Role  domain.Role
}

type rangeLoadDocument struct {
	Wire         domain.DocumentRepresentation
	Manifest     domain.ConversationManifest
	Events       []rangeLoadEvent
	StreamSHA256 string
}

func rangeLoadCanonicalEvent(event domain.CIREvent) ([]byte, error) {
	raw, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{event}})
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Events) != 1 {
		return nil, fmt.Errorf("canonical event decode: %v", err)
	}
	return decoded.Events[0], nil
}

func rangeLoadBuild(t *testing.T, name string, target int, consume func(domain.ContentHash, []byte) error) rangeLoadDocument {
	t.Helper()
	var result rangeLoadDocument
	streamHash := sha256.New()
	buffer := make([]byte, 0, domain.ConversationManifestChunkBytes)
	chunks := []domain.ConversationManifestChunk{}
	flush := func() {
		hash := domain.HashContent(buffer)
		if err := consume(hash, buffer); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, domain.ConversationManifestChunk{Hash: hash, Bytes: int64(len(buffer))})
		buffer = buffer[:0]
	}
	write := func(raw []byte) {
		streamHash.Write(raw)
		for len(raw) > 0 {
			count := min(len(raw), cap(buffer)-len(buffer))
			buffer = append(buffer, raw[:count]...)
			raw = raw[count:]
			if len(buffer) == cap(buffer) {
				flush()
			}
		}
	}
	streamBytes := 0
	for index := 0; streamBytes < target; index++ {
		event := domain.CIREvent{Seq: index, Kind: domain.EventMessage, Role: domain.RoleUser}
		text := fmt.Sprintf("%s event %06d: preserve UTF-8 \uac80\uc99d and exact content", name, index)
		payloadBytes := 4 << 10
		if index == 1 {
			payloadBytes = 256 << 10
		}
		if index >= 140 && index%10 == 5 {
			event.Role = domain.RoleAssistant
		}
		if index != 0 && (index < 140 || index%5 != 0) {
			event.Kind, event.Role = domain.EventToolResult, ""
			event.CallID = fmt.Sprintf("range-tool-%06d", index)
			line := fmt.Sprintf("%s tool %06d deterministic output \uac80\uc99d\n", name, index)
			text += strings.Repeat(line, payloadBytes/len(line)+1)
		}
		if event.Kind == domain.EventToolResult {
			event.Output = text
		} else {
			event.Blocks = []domain.ContentBlock{{Type: "text", Text: text}}
		}
		raw, err := rangeLoadCanonicalEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == domain.EventToolResult && len(raw) < payloadBytes {
			t.Fatal("tool fixture lost its output payload")
		}
		if index > 0 {
			write([]byte(","))
			streamBytes++
		}
		write(raw)
		streamBytes += len(raw)
		result.Events = append(result.Events, rangeLoadEvent{Hash: domain.HashContent(raw), Bytes: len(raw), Kind: event.Kind, Role: event.Role})
	}
	if len(buffer) > 0 {
		flush()
	}
	canonical, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: name}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err = json.Unmarshal(canonical, &envelope); err != nil {
		t.Fatal(err)
	}
	result.Manifest, err = domain.NewConversationManifest(envelope.Envelope, chunks, int64(len(result.Events)))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := domain.CanonicalConversationManifest(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	result.Wire = domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: manifest}
	result.StreamSHA256 = hex.EncodeToString(streamHash.Sum(nil))
	return result
}

func rangeLoadSave(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func TestDocRangeLoadFixtureRoundTrip(t *testing.T) {
	bodies := map[domain.ContentHash][]byte{}
	fixture := rangeLoadBuild(t, "range-fixture-self-check", 512<<10, func(hash domain.ContentHash, raw []byte) error {
		bodies[hash] = bytes.Clone(raw)
		return nil
	})
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := rangeLoadSave(path, fixture); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var restored rangeLoadDocument
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, fixture) || len(fixture.Events) < 20 || fixture.Events[1].Bytes < 256<<10 {
		t.Fatal("fixture roundtrip lost metadata or tool payload")
	}
	if err := domain.VerifyConversationManifest(context.Background(), restored.Wire.Hash, restored.Manifest, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return bodies[hash], nil
	}); err != nil {
		t.Fatal(err)
	}
	stream := []byte{}
	for _, chunk := range restored.Manifest.Chunks {
		stream = append(stream, bodies[chunk.Hash]...)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(append(append([]byte("["), stream...), ']'), &events); err != nil {
		t.Fatal(err)
	}
	for index, event := range events {
		digest := sha256.Sum256(event)
		if "sha256:"+hex.EncodeToString(digest[:]) != string(restored.Events[index].Hash) {
			t.Fatal("event digest convention mismatch")
		}
	}
}
