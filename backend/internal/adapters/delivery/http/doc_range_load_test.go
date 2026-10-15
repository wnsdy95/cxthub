//go:build postgres

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

type rangeLoadFixture struct {
	Run                 string
	Repo                domain.ContentHash
	Token               string
	Root                rangeLoadDocument
	Upload              rangeLoadDocument
	Snapshots           []domain.ContentHash
	Pending             map[string]domain.ContentHash
	UploadSource        string
	UploadPayloadHashes map[domain.ContentHash]domain.ContentHash
}

type rangeLoadSample struct {
	Phase        string
	Kind         string
	Status       int
	Bytes        int
	Start        time.Time
	End          time.Time
	Microseconds int64
	Error        string `json:",omitempty"`
}

type rangeLoadClient struct {
	client  *http.Client
	base    string
	token   string
	mu      sync.Mutex
	samples []rangeLoadSample
}

type rangeLoadMemoryRecorder struct {
	mu    sync.Mutex
	phase string
	stop  chan struct{}
	done  chan error
}

func (recorder *rangeLoadMemoryRecorder) setPhase(phase string) {
	recorder.mu.Lock()
	recorder.phase = phase
	recorder.mu.Unlock()
}

func rangeLoadRecordMemory(directory string) (*rangeLoadMemoryRecorder, error) {
	file, err := os.OpenFile(filepath.Join(directory, "memory-samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	recorder := &rangeLoadMemoryRecorder{phase: "startup", stop: make(chan struct{}), done: make(chan error, 1)}
	encoder := json.NewEncoder(file)
	record := func() error {
		recorder.mu.Lock()
		phase := recorder.phase
		recorder.mu.Unlock()
		cgroup := map[string]map[string]int64{}
		for _, name := range []string{"memory.current", "memory.peak", "memory.stat", "memory.events"} {
			raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", name))
			if err != nil {
				return err
			}
			values := map[string]int64{}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				fields := strings.Fields(line)
				key := "bytes"
				if len(fields) == 2 {
					key = fields[0]
				} else if len(fields) != 1 {
					return fmt.Errorf("invalid %s line %q", name, line)
				}
				value, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
				if err != nil {
					return err
				}
				values[key] = value
			}
			cgroup[name] = values
		}
		raw, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return err
		}
		process := map[string]int64{}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			key := strings.TrimSuffix(fields[0], ":")
			switch key {
			case "VmRSS", "VmHWM", "RssAnon", "RssFile", "RssShmem", "VmSwap":
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || fields[2] != "kB" {
					return fmt.Errorf("invalid process memory line %q", line)
				}
				process[key] = value * 1024
			}
		}
		if _, ok := process["VmHWM"]; !ok {
			return errors.New("process RSS high-water mark unavailable")
		}
		return encoder.Encode(map[string]any{"time": time.Now().UTC(), "phase": phase, "cgroup": cgroup, "process_bytes": process})
	}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := record(); err != nil {
				recorder.done <- errors.Join(err, file.Close())
				return
			}
			select {
			case <-recorder.stop:
				recorder.done <- errors.Join(record(), file.Close())
				return
			case <-ticker.C:
			}
		}
	}()
	return recorder, nil
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
		if index%10 != 0 {
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

func rangeLoadSetup(t *testing.T, ctx context.Context, storage *store.PostgresStore, directory, run, uploadSource string) {
	t.Helper()
	if _, err := storage.ApplyMigrations(ctx, os.Getenv("CXT_RANGE_MIGRATIONS")); err != nil {
		t.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	user, session, err := identity.Login(ctx, "dev:"+run+"@example.test", "range load")
	if err != nil {
		t.Fatal(err)
	}
	record, err := identity.CreateRepository(ctx, user, "RangeLoad")
	if err != nil {
		t.Fatal(err)
	}
	fixture := rangeLoadFixture{Run: run, Repo: domain.HashContent([]byte(record.ID)), Token: session.Token, Pending: map[string]domain.ContentHash{}, UploadSource: uploadSource, UploadPayloadHashes: map[domain.ContentHash]domain.ContentHash{}}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: fixture.Repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	putSnapshot := func(hash domain.ContentHash, identity domain.DocumentIdentity) {
		snapshot := domain.Snapshot{ID: hash, DocHash: hash, DocIdentity: identity, RepoID: fixture.Repo, Branch: "main", CreatedAt: created.Add(time.Duration(len(fixture.Snapshots)) * time.Second), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "deterministic range load", SessionID: fmt.Sprintf("range-session-%03d", len(fixture.Snapshots))}
		if len(fixture.Snapshots) > 0 {
			snapshot.Parents = []domain.ContentHash{fixture.Snapshots[len(fixture.Snapshots)-1]}
		}
		if err := storage.PutSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		fixture.Snapshots = append(fixture.Snapshots, hash)
	}
	for index := 0; index < 699; index++ {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("range graph snapshot %04d", index)}}}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, fixture.Repo, doc); err != nil {
			t.Fatal(err)
		}
		putSnapshot(doc.Hash, domain.DocumentIdentityLegacy)
	}
	if err := storage.RequireDocumentIdentity(ctx, fixture.Repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	fixture.Root = rangeLoadBuild(t, "range-root-v1", 310<<20, func(hash domain.ContentHash, raw []byte) error {
		_, _, err := storage.PutChunks(ctx, fixture.Repo, map[domain.ContentHash][]byte{hash: raw})
		return err
	})
	job, err := domain.NewDocFinalizationJobForRepresentation(fixture.Repo, fixture.Root.Wire, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.EnqueueDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	if err = service.ProcessDocFinalizations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	completed, err := storage.GetDocJob(ctx, fixture.Repo, job.ID)
	if err != nil || completed.State != "completed" {
		t.Fatalf("setup publication: state=%s err=%v", completed.State, err)
	}
	putSnapshot(fixture.Root.Wire.Hash, domain.DocumentIdentityRootV1)
	if _, err := storage.DocRangeIndex(ctx, fixture.Repo, fixture.Root.Wire.Hash); err != nil {
		t.Fatalf("setup publication did not create range index: %v", err)
	}
	if err := storage.CompareAndSwapRef(ctx, fixture.Repo, domain.Ref{RepoID: fixture.Repo, Kind: domain.RefBranch, Name: "main", Target: fixture.Root.Wire.Hash}, ""); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 20; index++ {
		pending := domain.Pending{RepoID: fixture.Repo, SessionID: fmt.Sprintf("pending-%02d", index), Branch: "main", Target: fixture.Snapshots[len(fixture.Snapshots)-1-index], Provider: domain.ProviderCodex, Author: domain.TeamIdentity{Email: user.Email}, UpdatedAt: created.Add(time.Hour)}
		if err := storage.PutPending(ctx, fixture.Repo, pending); err != nil {
			t.Fatal(err)
		}
		fixture.Pending[pending.SessionID] = pending.Target
	}
	var payloadPool *pgxpool.Pool
	if uploadSource == "postgres" {
		payloadPool, err = pgxpool.New(ctx, os.Getenv("CXT_RANGE_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		defer payloadPool.Close()
		if _, err := payloadPool.Exec(ctx, `CREATE TABLE range_load_upload_payloads (hash text PRIMARY KEY, body bytea NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 1048576))`); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.Mkdir(filepath.Join(directory, "chunks"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	workerMiB, err := strconv.Atoi(os.Getenv("CXT_RANGE_WORKER_MIB"))
	if err != nil || (workerMiB != 32 && workerMiB != 310) {
		t.Fatal("CXT_RANGE_WORKER_MIB must be 32 or 310")
	}
	fixture.Upload = rangeLoadBuild(t, fmt.Sprintf("range-upload-%d-v1", workerMiB), workerMiB<<20, func(hash domain.ContentHash, raw []byte) error {
		body, err := json.Marshal(chunksBody{Chunks: []inbound.ChunkObject{{Hash: hash, Data: raw}}})
		if err != nil {
			return err
		}
		if len(body) > 1<<20 {
			return fmt.Errorf("upload JSON exceeds 1MiB: %d", len(body))
		}
		fixture.UploadPayloadHashes[hash] = domain.HashContent(body)
		if payloadPool != nil {
			_, err := payloadPool.Exec(ctx, `INSERT INTO range_load_upload_payloads(hash,body) VALUES($1,$2)`, hash, body)
			return err
		}
		return os.WriteFile(filepath.Join(directory, "chunks", string(hash)+".json"), body, 0600)
	})
	if err := rangeLoadSave(filepath.Join(directory, "fixture.json"), fixture); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded outside capped process: root_stream_bytes=%d root_events=%d root_chunks=%d snapshots=%d pending=%d upload_stream_bytes=%d upload_chunks=%d; deterministic compressible synthetic tools", fixture.Root.Manifest.StreamBytes, len(fixture.Root.Events), len(fixture.Root.Manifest.Chunks), len(fixture.Snapshots), len(fixture.Pending), fixture.Upload.Manifest.StreamBytes, len(fixture.Upload.Manifest.Chunks))
	t.Log("graph limitation: 700 linear snapshots, one main branch ref, no PR/merge topology; production graph concurrency is not established")
	t.Logf("upload payload source=%s; exact HTTP-body hashes recorded; each body <=1MiB; generation outside application cgroup", uploadSource)
}

func (client *rangeLoadClient) request(ctx context.Context, phase, kind, method, path string, body []byte, expected int) ([]byte, error) {
	for attempt := 0; attempt < 64; attempt++ {
		sample := rangeLoadSample{Phase: phase, Kind: kind, Start: time.Now()}
		request, err := http.NewRequestWithContext(ctx, method, client.base+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+client.token)
		request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.client.Do(request)
		var raw []byte
		retry := false
		if err == nil {
			sample.Status = response.StatusCode
			raw, err = io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
			err = errors.Join(err, response.Body.Close())
			sample.Bytes = len(raw)
			if err == nil && (len(raw) > 4<<20 || !json.Valid(raw)) {
				err = fmt.Errorf("oversized or invalid JSON: bytes=%d", len(raw))
			}
			if err == nil && response.StatusCode == http.StatusServiceUnavailable {
				var failure struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				err = json.Unmarshal(raw, &failure)
				retry = err == nil && failure.Error.Code == "resource_overloaded" && response.Header.Get("Retry-After") == "1"
				if !retry {
					err = fmt.Errorf("unexpected 503: %s", raw)
				}
			} else if err == nil && response.StatusCode != expected {
				err = fmt.Errorf("HTTP %d expected %d: %.500s", response.StatusCode, expected, raw)
			}
		}
		sample.End = time.Now()
		sample.Microseconds = sample.End.Sub(sample.Start).Microseconds()
		if err != nil {
			sample.Error = err.Error()
		}
		client.mu.Lock()
		client.samples = append(client.samples, sample)
		client.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", kind, path, err)
		}
		if !retry {
			return raw, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("%s exhausted 64 retryable admissions", kind)
}

func rangeLoadCheckPage(raw []byte, doc rangeLoadDocument, offset, limit int, view string) error {
	var page struct {
		Hash   domain.ContentHash `json:"hash"`
		Total  int                `json:"total"`
		Offset int                `json:"offset"`
		Next   int                `json:"next"`
		Events []json.RawMessage  `json:"events"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return err
	}
	if page.Hash != doc.Wire.Hash || page.Total != len(doc.Events) || page.Offset != offset || len(page.Events) == 0 || len(raw) > 256<<10 {
		return fmt.Errorf("invalid %s page identity/range/budget", view)
	}
	visible := 0
	for ordinal, item := range page.Events {
		var event domain.CIREvent
		var metadata struct {
			Index    int  `json:"event_index"`
			Deferred bool `json:"deferred"`
			Bytes    int  `json:"event_bytes"`
		}
		if err := json.Unmarshal(item, &event); err != nil {
			return err
		}
		if err := json.Unmarshal(item, &metadata); err != nil {
			return err
		}
		position := offset + ordinal
		if position >= len(doc.Events) || metadata.Index != position {
			return fmt.Errorf("event cursor mismatch")
		}
		want := doc.Events[position]
		if event.Seq != position || event.Kind != want.Kind || event.Role != want.Role || metadata.Bytes != want.Bytes {
			return fmt.Errorf("event metadata mismatch at %d", position)
		}
		if metadata.Deferred {
			if len(event.Blocks) != 0 || (want.Kind == domain.EventMessage && want.Bytes <= 128<<10) {
				return fmt.Errorf("invalid lazy event at %d", position)
			}
		} else {
			canonical, err := rangeLoadCanonicalEvent(event)
			if err != nil || domain.HashContent(canonical) != want.Hash {
				return fmt.Errorf("inline event checksum mismatch at %d: %v", position, err)
			}
		}
		if view == "raw" || want.Kind == domain.EventMessage {
			visible++
		}
	}
	end := offset + len(page.Events)
	wantNext := end
	if end == len(doc.Events) {
		wantNext = -1
	}
	if page.Next != wantNext || visible > limit || (end < len(doc.Events) && visible != limit) {
		return fmt.Errorf("page continuation/visible limit mismatch: next=%d visible=%d", page.Next, visible)
	}
	return nil
}

func rangeLoadCheckGraph(raw []byte, fixture rangeLoadFixture) error {
	var graph struct {
		Encoding    string               `json:"encoding"`
		Dictionary  []domain.ContentHash `json:"dictionary"`
		SnapshotIDs []int                `json:"snapshot_ids"`
	}
	if err := json.Unmarshal(raw, &graph); err != nil {
		return err
	}
	if graph.Encoding != "indexed-v2" || len(graph.SnapshotIDs) != len(fixture.Snapshots) {
		return fmt.Errorf("graph encoding/count: %s/%d expected %d", graph.Encoding, len(graph.SnapshotIDs), len(fixture.Snapshots))
	}
	want := map[domain.ContentHash]bool{}
	for _, hash := range fixture.Snapshots {
		want[hash] = true
	}
	for _, index := range graph.SnapshotIDs {
		if index < 0 || index >= len(graph.Dictionary) || !want[graph.Dictionary[index]] {
			return fmt.Errorf("graph unexpected/duplicate snapshot")
		}
		delete(want, graph.Dictionary[index])
	}
	return nil
}

func (client *rangeLoadClient) read(ctx context.Context, phase, kind string, fixture rangeLoadFixture, iteration int) error {
	path := "/docs/" + string(fixture.Root.Wire.Hash)
	if kind == "chat" || kind == "raw" {
		offset := 0
		if iteration%3 == 1 {
			offset = 200
		} else if iteration%3 == 2 {
			offset = max(0, len(fixture.Root.Events)-200)
		}
		raw, err := client.request(ctx, phase, kind, "GET", fmt.Sprintf("%s/events?view=%s&offset=%d&limit=20", path, kind, offset), nil, 200)
		if err != nil {
			return err
		}
		return rangeLoadCheckPage(raw, fixture.Root, offset, 20, kind)
	}
	if kind == "fragment" {
		index := 1
		if iteration%2 != 0 {
			index = len(fixture.Root.Events) - 1
		}
		digest := sha256.New()
		offset := 0
		for fragmentCount := 0; fragmentCount < 16; fragmentCount++ {
			raw, err := client.request(ctx, phase, kind, "GET", fmt.Sprintf("%s/event-fragments?index=%d&offset=%d&budget=65536", path, index, offset), nil, 200)
			if err != nil {
				return err
			}
			var fragment domain.DocViewFragment
			if err := json.Unmarshal(raw, &fragment); err != nil {
				return err
			}
			if fragment.Index != index || fragment.Offset != offset || fragment.Total != fixture.Root.Events[index].Bytes || len(fragment.JSON) == 0 || len(fragment.JSON) > 64<<10 {
				return fmt.Errorf("fragment identity/budget mismatch")
			}
			digest.Write([]byte(fragment.JSON))
			offset += len(fragment.JSON)
			if fragment.Next == -1 {
				got := "sha256:" + hex.EncodeToString(digest.Sum(nil))
				if offset != fragment.Total || got != string(fixture.Root.Events[index].Hash) {
					return fmt.Errorf("fragment reconstruction mismatch: index=%d bytes=%d/%d hash=%s want=%s", index, offset, fragment.Total, got, fixture.Root.Events[index].Hash)
				}
				return nil
			}
			if fragment.Next != offset {
				return fmt.Errorf("non-progressing fragment cursor")
			}
		}
		return fmt.Errorf("fragment request bound exhausted")
	}
	if kind == "graph" {
		raw, err := client.request(ctx, phase, kind, "GET", "/graph-state?graph_encoding=indexed-v2", nil, 200)
		if err != nil {
			return err
		}
		return rangeLoadCheckGraph(raw, fixture)
	}
	raw, err := client.request(ctx, phase, kind, "GET", "/pending-view?projection=on-hold-v1", nil, 200)
	if err != nil {
		return err
	}
	var pending domain.PendingStateView
	if err := json.Unmarshal(raw, &pending); err != nil {
		return err
	}
	actual := map[string]domain.ContentHash{}
	for _, item := range pending.Pending {
		if _, duplicate := actual[item.SessionID]; duplicate || item.RepoID != fixture.Repo {
			return fmt.Errorf("pending identity mismatch")
		}
		actual[item.SessionID] = item.Target
	}
	if !reflect.DeepEqual(actual, fixture.Pending) {
		return fmt.Errorf("pending payload changed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, fullGraph := fields["graph"]; fullGraph || pending.Projection != "on-hold-v1" || pending.DefaultBranch != "main" || len(pending.Refs) != 1 || pending.Refs[0].Target != fixture.Root.Wire.Hash {
		return fmt.Errorf("light pending projection identity/shape mismatch")
	}
	wantTargets := map[domain.ContentHash]bool{}
	for _, hash := range fixture.Pending {
		wantTargets[hash] = true
	}
	for _, snapshot := range pending.Snapshots {
		if !wantTargets[snapshot.ID] || snapshot.DocHash != snapshot.ID || snapshot.RepoID != fixture.Repo || len(snapshot.Branches) != 0 {
			return fmt.Errorf("pending capture metadata mismatch")
		}
		delete(wantTargets, snapshot.ID)
	}
	if len(wantTargets) != 0 || len(pending.Hold) != 0 || len(pending.OrphanSessions) != 0 || len(pending.HoldCounts) != 0 {
		return fmt.Errorf("pending classification mismatch for main-reachable fixture")
	}
	return nil
}

func rangeLoadMix(ctx context.Context, client *rangeLoadClient, fixture rangeLoadFixture, phase string, work func() error) error {
	start := make(chan struct{})
	errorsFound := make(chan error, 21)
	var group sync.WaitGroup
	group.Add(21)
	go func() { defer group.Done(); <-start; errorsFound <- work() }()
	kinds := []string{"chat", "raw", "fragment", "graph", "pending"}
	for actor := 0; actor < 20; actor++ {
		go func(actor int) {
			defer group.Done()
			<-start
			for iteration := 0; iteration < 8; iteration++ {
				if err := client.read(ctx, phase, kinds[(actor+iteration)%len(kinds)], fixture, iteration); err != nil {
					errorsFound <- err
					return
				}
			}
		}(actor)
	}
	close(start)
	group.Wait()
	close(errorsFound)
	var result error
	for err := range errorsFound {
		result = errors.Join(result, err)
	}
	return result
}

type rangeLoadWorkerStore struct {
	*store.PostgresStore
	claimed chan time.Time
}

func (storage *rangeLoadWorkerStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	job, err := storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
	if err == nil {
		storage.claimed <- time.Now()
	}
	return job, err
}

func rangeLoadCgroup(t *testing.T) {
	t.Helper()
	for name, expected := range map[string]string{"memory.max": "536870912", "memory.swap.max": "0"} {
		raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", name))
		if err != nil || strings.TrimSpace(string(raw)) != expected {
			t.Fatalf("512MiB/no-swap cgroup required: %s=%q err=%v", name, raw, err)
		}
	}
	raw, err := os.ReadFile("/sys/fs/cgroup/memory.peak")
	if err != nil {
		t.Fatal(err)
	}
	peak, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || peak <= 0 {
		t.Fatalf("invalid cgroup peak=%d err=%v", peak, err)
	}
	events, err := os.ReadFile("/sys/fs/cgroup/memory.events")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cgroup peak_bytes=%d memory.max=536870912 memory.swap.max=0 headroom_bytes=%d GOMEMLIMIT=%q memory.events=%q", peak, (512<<20)-peak, os.Getenv("GOMEMLIMIT"), strings.TrimSpace(string(events)))
	for _, line := range strings.Split(strings.TrimSpace(string(events)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == "oom" || fields[0] == "oom_kill" || fields[0] == "oom_group_kill") && fields[1] != "0" {
			t.Fatalf("cgroup OOM event: %s", line)
		}
	}
	if peak > 512<<20 {
		t.Fatalf("cgroup peak=%d exceeds strict 512MiB cap", peak)
	}
}

func rangeLoadCold(t *testing.T, ctx context.Context, dsn, directory string, client *rangeLoadClient, storage *store.PostgresStore, fixture rangeLoadFixture) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	header, err := storage.DocRangeIndex(ctx, fixture.Repo, fixture.Root.Wire.Hash)
	if err != nil || !reflect.DeepEqual(header.Manifest, fixture.Root.Manifest) {
		t.Fatalf("cold fixture must start with published range index: %v", err)
	}
	hashes := []string{string(fixture.Root.Wire.Hash)}
	for _, chunk := range fixture.Root.Manifest.Chunks {
		hashes = append(hashes, string(chunk.Hash))
	}
	sourceFingerprint := func() string {
		var count int
		var fingerprint string
		err := pool.QueryRow(ctx, `SELECT count(*), md5(string_agg(rb.kind || ':' || rb.hash || ':' || octet_length(b.bytes)::text || ':' || md5(b.bytes), E'\n' ORDER BY rb.kind, rb.hash))
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1 AND rb.hash=ANY($2::text[])`, fixture.Repo, hashes).Scan(&count, &fingerprint)
		if err != nil || count != len(hashes) {
			t.Fatalf("cold source/grant fingerprint: rows=%d expected=%d err=%v", count, len(hashes), err)
		}
		return fingerprint
	}
	before := sourceFingerprint()
	deleted, err := pool.Exec(ctx, `DELETE FROM doc_range_indexes_v1 WHERE hash=$1
 AND EXISTS (SELECT 1 FROM repo_blobs WHERE repo_id=$2 AND kind='doc' AND hash=$1)`, fixture.Root.Wire.Hash, fixture.Repo)
	if err != nil || deleted.RowsAffected() != 1 {
		t.Fatalf("cold preparation must delete exactly one derived header: rows=%d err=%v", deleted.RowsAffected(), err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$1) + (SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$1)`, fixture.Root.Wire.Hash).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cold derived index deletion incomplete: rows=%d err=%v", remaining, err)
	}
	if _, err := storage.DocRangeIndex(ctx, fixture.Repo, fixture.Root.Wire.Hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatalf("cold root unexpectedly still indexed: %v", err)
	}
	if sourceFingerprint() != before {
		t.Fatal("derived index deletion changed source blobs or grants")
	}
	t.Logf("cold preparation deleted only disposable root derived header/pages; %d source/grant rows unchanged; PostgreSQL buffers are not cold", len(hashes))
	path := "/docs/" + string(fixture.Root.Wire.Hash) + "/events?view=chat&offset=0&limit=20"
	started := time.Now()
	raw, err := client.request(ctx, "cold", "chat", "GET", path, nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	coldElapsed := time.Since(started)
	if err := rangeLoadCheckPage(raw, fixture.Root, 0, 20, "chat"); err != nil {
		t.Fatal(err)
	}
	peak, err := os.ReadFile("/sys/fs/cgroup/memory.peak")
	if err != nil {
		t.Fatal(err)
	}
	if err := rangeLoadSave(filepath.Join(directory, "cold-samples.json"), client.samples); err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated cold first chat page=%s cumulative_peak_bytes=%s; verifying rebuilt index before sequential warm request", coldElapsed, strings.TrimSpace(string(peak)))
	rebuilt, err := storage.DocRangeIndex(ctx, fixture.Repo, fixture.Root.Wire.Hash)
	if err != nil || !reflect.DeepEqual(rebuilt, header) {
		t.Fatalf("cold backfill changed authenticated range header: %v", err)
	}
	verified := 0
	for page := range rebuilt.Pages {
		entries, err := storage.DocRangePage(ctx, fixture.Repo, rebuilt, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if verified >= len(fixture.Root.Events) {
				t.Fatal("cold backfill has excess events")
			}
			want := fixture.Root.Events[verified]
			if entry.Hash != want.Hash || entry.Length != want.Bytes || entry.Seq != verified || entry.Kind != want.Kind {
				t.Fatalf("cold backfill event checksum mismatch at %d", verified)
			}
			verified++
		}
	}
	if verified != len(fixture.Root.Events) || sourceFingerprint() != before {
		t.Fatal("cold backfill event count/source/grant preservation mismatch")
	}
	started = time.Now()
	raw, err = client.request(ctx, "warm", "chat", "GET", path, nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	warmElapsed := time.Since(started)
	if err := rangeLoadCheckPage(raw, fixture.Root, 0, 20, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := rangeLoadSave(filepath.Join(directory, "cold-proof.json"), map[string]any{"root": fixture.Root.Wire.Hash, "deleted_headers": 1, "source_grant_rows": len(hashes), "source_grant_fingerprint": before, "verified_events": verified, "cold_microseconds": coldElapsed.Microseconds(), "cold_cumulative_peak_bytes": strings.TrimSpace(string(peak)), "warm_microseconds": warmElapsed.Microseconds()}); err != nil {
		t.Fatal(err)
	}
	t.Logf("PASS: isolated cold backfill=%s warm=%s; all %d event checksums and source/grant preservation verified; cold derived index only, one sample each, not disk-cold or SLA evidence", coldElapsed, warmElapsed, verified)
}

func TestPostgresDocRangeMixedLoad(t *testing.T) {
	phase := os.Getenv("CXT_RANGE_PHASE")
	if phase == "" {
		t.Skip("opt-in: bash scripts/test-doc-range-load.sh creates disposable local PostgreSQL")
	}
	if phase != "setup" && phase != "load" && phase != "baseline" && phase != "cold" {
		t.Fatal("CXT_RANGE_PHASE must be setup, load, baseline or cold")
	}
	uploadSource := os.Getenv("CXT_RANGE_UPLOAD_SOURCE")
	if uploadSource == "" {
		uploadSource = "files"
	}
	if uploadSource != "files" && uploadSource != "postgres" {
		t.Fatal("CXT_RANGE_UPLOAD_SOURCE must be files or postgres")
	}
	dsn, directory, run := os.Getenv("CXT_RANGE_DSN"), os.Getenv("CXT_RANGE_ARTIFACTS"), os.Getenv("CXT_RANGE_RUN")
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil || parsed.User.Username() != "range_load" || parsed.Path != "/cxthub_range_load" || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "range-postgres") || parsed.Query().Get("sslmode") != "disable" || os.Getenv("CXT_RANGE_DISPOSABLE") != "1" || directory == "" || !strings.HasPrefix(run, "cxthub-range-") {
		t.Fatal("requires explicit disposable local range_load/cxthub_range_load DSN and run/artifact settings")
	}
	ctx, cancel := context.WithTimeout(inbound.WithDocumentIdentities(systemTestContext(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), 12*time.Minute)
	defer cancel()
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, identities, identities)
	if phase == "setup" {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&count)
		pool.Close()
		if err != nil || count != 0 {
			t.Fatalf("setup refuses nonempty database: tables=%d err=%v", count, err)
		}
	} else {
		rangeLoadCgroup(t)
		defer rangeLoadCgroup(t)
	}
	var memory *rangeLoadMemoryRecorder
	if phase != "setup" {
		memory, err = rangeLoadRecordMemory(directory)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			close(memory.stop)
			if err := <-memory.done; err != nil {
				t.Errorf("memory sampler: %v", err)
			}
		}()
	}
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if phase == "setup" {
		rangeLoadSetup(t, ctx, storage, directory, run, uploadSource)
		return
	}
	raw, err := os.ReadFile(filepath.Join(directory, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture rangeLoadFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Run != run || fixture.Root.Manifest.StreamBytes < 310<<20 || fixture.Upload.Manifest.StreamBytes < 32<<20 || len(fixture.Snapshots) != 700 || fixture.UploadSource != uploadSource || len(fixture.UploadPayloadHashes) != len(fixture.Upload.Manifest.Chunks) {
		t.Fatal("fixture provenance/size mismatch")
	}
	service := app.NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	if err := service.ConfigureConversationRootPublication(true); err != nil {
		t.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	server := httptest.NewServer(NewServer(service, identity).Handler())
	defer server.Close()
	client := &rangeLoadClient{client: &http.Client{Timeout: 45 * time.Second}, base: server.URL + "/api/v1/repos/" + string(fixture.Repo), token: fixture.Token}
	defer client.client.CloseIdleConnections()
	functionalComplete := false
	defer func() {
		if err := rangeLoadSave(filepath.Join(directory, "functional-result.json"), map[string]any{"phase": phase, "completed": functionalComplete, "passed": functionalComplete && !t.Failed()}); err != nil {
			t.Error(err)
		}
	}()
	defer func() {
		if err := rangeLoadSave(filepath.Join(directory, "samples.json"), client.samples); err != nil {
			t.Error(err)
		}
		groups := map[string][]int64{}
		maximum := map[string]int{}
		for _, sample := range client.samples {
			key := fmt.Sprintf("%s/%s/HTTP%d", sample.Phase, sample.Kind, sample.Status)
			groups[key] = append(groups[key], sample.Microseconds)
			maximum[key] = max(maximum[key], sample.Bytes)
		}
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			values := groups[key]
			sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
			t.Logf("%s attempts=%d p50_us=%d p95_us=%d max_us=%d max_response_bytes=%d", key, len(values), values[(len(values)-1)/2], values[(len(values)-1)*95/100], values[len(values)-1], maximum[key])
		}
	}()
	if phase == "cold" {
		memory.setPhase("cold")
		rangeLoadCold(t, ctx, dsn, directory, client, storage, fixture)
		functionalComplete = true
		return
	}
	if phase == "baseline" {
		memory.setPhase("baseline")
		path := "/docs/" + string(fixture.Root.Wire.Hash) + "/events?offset=0&limit=20"
		started := time.Now()
		raw, err := client.request(ctx, "baseline", "chat", "GET", path+"&view=chat", nil, 200)
		if err != nil {
			t.Fatal(err)
		}
		if err := rangeLoadCheckPage(raw, fixture.Root, 0, 20, "chat"); err != nil {
			t.Fatal(err)
		}
		chatElapsed := time.Since(started)
		if err := rangeLoadSave(filepath.Join(directory, "baseline-chat-samples.json"), client.samples); err != nil {
			t.Fatal(err)
		}
		t.Logf("isolated chat first page=%s; starting no-view strict full-audit request without concurrent workload", chatElapsed)
		started = time.Now()
		raw, err = client.request(ctx, "baseline", "strict", "GET", path, nil, 200)
		if err != nil {
			t.Fatal(err)
		}
		strictElapsed := time.Since(started)
		var strict domain.DocEventPage
		if err := json.Unmarshal(raw, &strict); err != nil {
			t.Fatal(err)
		}
		if strict.Hash != fixture.Root.Wire.Hash || strict.Total != len(fixture.Root.Events) || strict.Offset != 0 || strict.Next != 20 || len(strict.Events) != 20 {
			t.Fatal("strict baseline page identity/continuation mismatch")
		}
		for position, event := range strict.Events {
			canonical, err := rangeLoadCanonicalEvent(event)
			if err != nil || domain.HashContent(canonical) != fixture.Root.Events[position].Hash {
				t.Fatalf("strict baseline checksum mismatch at %d: %v", position, err)
			}
		}
		t.Logf("PASS: isolated baseline chat=%s strict=%s; one sample each, chat first, same corpus, different payload contracts; not an SLA or cold-cache equivalence", chatElapsed, strictElapsed)
		functionalComplete = true
		return
	}
	var payloadPool *pgxpool.Pool
	if uploadSource == "postgres" {
		payloadPool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer payloadPool.Close()
	}
	t.Logf("controlled upload input=%s; payload bodies individually hash-verified and bounded to1MiB", uploadSource)
	var uploadStart, uploadEnd time.Time
	memory.setPhase("upload")
	if err := rangeLoadMix(ctx, client, fixture, "upload", func() error {
		uploadStart = time.Now()
		defer func() { uploadEnd = time.Now() }()
		for _, chunk := range fixture.Upload.Manifest.Chunks {
			var body []byte
			var err error
			if payloadPool != nil {
				err = payloadPool.QueryRow(ctx, `SELECT body FROM range_load_upload_payloads WHERE hash=$1 AND octet_length(body)<=1048576`, chunk.Hash).Scan(&body)
			} else {
				body, err = os.ReadFile(filepath.Join(directory, "chunks", string(chunk.Hash)+".json"))
			}
			if err != nil {
				return err
			}
			if len(body) == 0 || len(body) > 1<<20 || domain.HashContent(body) != fixture.UploadPayloadHashes[chunk.Hash] {
				return fmt.Errorf("upload payload checksum/buffer bound mismatch for %s", chunk.Hash)
			}
			if _, err := client.request(ctx, "upload", "chunk_upload", "POST", "/push/chunks", body, 200); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := rangeLoadSave(filepath.Join(directory, "upload-samples.json"), client.samples); err != nil {
		t.Fatal(err)
	}
	t.Logf("chunk upload overlap complete; starting same-process root worker: stream_bytes=%d", fixture.Upload.Manifest.StreamBytes)
	memory.setPhase("worker_submit")
	body, err := json.Marshal(fixture.Upload.Wire)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = client.request(ctx, "worker", "submit", "POST", "/push/doc-jobs", body, 202)
	if err != nil {
		t.Fatal(err)
	}
	var accepted inbound.DocFinalizationStatus
	if err := json.Unmarshal(raw, &accepted); err != nil || accepted.State != "waiting" || accepted.DocHash != fixture.Upload.Wire.Hash {
		t.Fatalf("bad acceptance: %+v %v", accepted, err)
	}
	workerPool, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer workerPool.Close()
	observed := &rangeLoadWorkerStore{PostgresStore: workerPool, claimed: make(chan time.Time, 1)}
	worker := app.NewService(observed, observed, nil, gitengine.NewEngine(observed), observed)
	var workerEnd time.Time
	workerAdmissionRetries := 0
	memory.setPhase("worker")
	if err := rangeLoadMix(ctx, client, fixture, "worker", func() error {
		defer func() {
			workerEnd = time.Now()
			memory.setPhase("worker_done")
		}()
		for attempt := 0; attempt < 16; attempt++ {
			err := worker.ProcessDocFinalizations(ctx, 1)
			if !errors.Is(err, domain.ErrResourceOverloaded) {
				return err
			}
			workerAdmissionRetries++
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		return fmt.Errorf("worker admission retries exhausted")
	}); err != nil {
		t.Fatal(err)
	}
	var workerStart time.Time
	select {
	case workerStart = <-observed.claimed:
	default:
		t.Fatal("worker never claimed the job")
	}
	t.Logf("worker_admission_retries=%d", workerAdmissionRetries)
	if err := rangeLoadSave(filepath.Join(directory, "work-windows.json"), map[string][2]time.Time{"upload": {uploadStart, uploadEnd}, "worker": {workerStart, workerEnd}}); err != nil {
		t.Fatal(err)
	}
	for phase, window := range map[string][2]time.Time{"upload": {uploadStart, uploadEnd}, "worker": {workerStart, workerEnd}} {
		overlap, completedDuring := map[string]int{}, map[string]int{}
		for _, sample := range client.samples {
			if sample.Phase != phase || sample.Kind == "chunk_upload" || sample.Kind == "submit" {
				continue
			}
			if sample.Start.Before(window[1]) && sample.End.After(window[0]) {
				overlap[sample.Kind]++
			}
			if sample.Status == 200 && sample.End.After(window[0]) && sample.End.Before(window[1]) {
				completedDuring[sample.Kind]++
			}
		}
		for _, kind := range []string{"chat", "raw", "fragment", "graph", "pending"} {
			if overlap[kind] == 0 {
				t.Errorf("%s did not overlap %s", kind, phase)
			}
		}
		if completedDuring["chat"] == 0 {
			t.Errorf("no successful chat page completed during %s", phase)
		}
		t.Logf("%s duration=%s overlapping_attempts=%v successful_completions_during=%v", phase, window[1].Sub(window[0]), overlap, completedDuring)
	}
	memory.setPhase("verification")
	raw, err = client.request(ctx, "verify", "status", "GET", "/push/doc-jobs/"+accepted.ID, nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	var completed inbound.DocFinalizationStatus
	if err := json.Unmarshal(raw, &completed); err != nil || completed.ID != accepted.ID || completed.State != "completed" || completed.DocHash != fixture.Upload.Wire.Hash {
		t.Fatalf("worker did not complete: %+v err=%v", completed, err)
	}
	header, err := storage.DocRangeIndex(ctx, fixture.Repo, fixture.Upload.Wire.Hash)
	if err != nil || !reflect.DeepEqual(header.Manifest, fixture.Upload.Manifest) {
		t.Fatalf("published range manifest mismatch: %v", err)
	}
	for page := range header.Pages {
		entries, err := storage.DocRangePage(ctx, fixture.Repo, header, page)
		if err != nil {
			t.Fatal(err)
		}
		for ordinal, entry := range entries {
			position := page*domain.DocRangePageEvents + ordinal
			if position >= len(fixture.Upload.Events) {
				t.Fatal("published range index has excess events")
			}
			want := fixture.Upload.Events[position]
			if entry.Hash != want.Hash || entry.Length != want.Bytes || entry.Seq != position || entry.Kind != want.Kind {
				t.Fatalf("published range event checksum mismatch at %d", position)
			}
		}
	}
	digest := sha256.New()
	for _, chunk := range fixture.Upload.Manifest.Chunks {
		raw, err := storage.ReadConversationChunk(ctx, fixture.Repo, chunk.Hash, chunk.Bytes)
		if err != nil || domain.HashContent(raw) != chunk.Hash {
			t.Fatalf("uploaded chunk checksum mismatch: %v", err)
		}
		digest.Write(raw)
	}
	if hex.EncodeToString(digest.Sum(nil)) != fixture.Upload.StreamSHA256 {
		t.Fatal("completed upload stream checksum mismatch")
	}
	snapshot := domain.Snapshot{ID: fixture.Upload.Wire.Hash, DocHash: fixture.Upload.Wire.Hash, DocIdentity: domain.DocumentIdentityRootV1, RepoID: fixture.Repo, CreatedAt: time.Now().UTC(), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	body, err = json.Marshal(objectsBody{Snapshots: []domain.Snapshot{snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.request(ctx, "verify", "snapshot", "POST", "/push/objects", body, 200); err != nil {
		t.Fatal(err)
	}
	raw, err = client.request(ctx, "verify", "uploaded_page", "GET", "/docs/"+string(snapshot.ID)+"/events?view=chat&offset=0&limit=20", nil, 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := rangeLoadCheckPage(raw, fixture.Upload, 0, 20, "chat"); err != nil {
		t.Fatal(err)
	}
	t.Logf("PASS: response identities, inline/event-fragment checksums, worker completion, published range index and full uploaded stream SHA256=%s; timings are observations, not SLA guarantees", fixture.Upload.StreamSHA256)
	functionalComplete = true
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
