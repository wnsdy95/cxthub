package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRootStagingVersionsBindIdentityAndKeepLegacyBytes(t *testing.T) {
	h := HashContent([]byte("fixture"))
	e := StagedSession{Provider: ProviderCodex, SessionID: "synthetic", SourceID: h, Generation: h, DocHash: h, CodeCommit: strings.Repeat("a", 40), BranchID: "main-id", CapturedAt: time.Unix(1, 0).UTC()}
	e.Key = StagedSessionKey(e.Provider, e.SessionID, e.SourceID, e.Generation)
	index := (StagingIndex{Version: 1, RepoID: string(h), WorktreeID: strings.Repeat("b", 32), Entries: []StagedSession{e}}).WithRevision()
	raw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy serialization retains v1 and has no newly introduced identity field.
	if strings.Contains(string(raw), "doc_identity") || index.Version != 1 || index.CommitVersion() != 2 {
		t.Fatal(string(raw))
	}
	// Freeze old field order and bytes independently of StagedSession's new field.
	type legacyEntry struct {
		Key           ContentHash  `json:"key"`
		Provider      ProviderKind `json:"provider"`
		SessionID     string       `json:"session_id"`
		SourceID      ContentHash  `json:"source_id"`
		Generation    ContentHash  `json:"generation"`
		DocHash       ContentHash  `json:"doc_hash"`
		Events        int          `json:"events"`
		StartEvent    int          `json:"start_event"`
		CapturedBytes int64        `json:"captured_bytes"`
		CodeCommit    string       `json:"code_commit"`
		Branch        string       `json:"branch"`
		BranchID      string       `json:"branch_id"`
		Base          ContentHash  `json:"base,omitempty"`
		CapturedAt    time.Time    `json:"captured_at"`
	}
	old, _ := json.Marshal(legacyEntry{Key: e.Key, Provider: e.Provider, SessionID: e.SessionID, SourceID: e.SourceID, Generation: e.Generation, DocHash: e.DocHash, CodeCommit: e.CodeCommit, BranchID: e.BranchID, CapturedAt: e.CapturedAt})
	now, _ := json.Marshal(e)
	if string(old) != string(now) {
		t.Fatal("legacy staged bytes changed")
	}
	index.Entries[0].DocIdentity = DocumentIdentityRootV1
	index = index.WithRevision()
	if index.Version != 2 || index.CommitVersion() != 3 || ValidateStagingIndex(index) != nil {
		t.Fatal(index)
	}
	rootRaw, _ := json.Marshal(index)
	var decoded StagingIndex
	if err := json.Unmarshal(rootRaw, &decoded); err != nil || ValidateStagingIndex(decoded) != nil || decoded.Entries[0].DocumentRef() != index.Entries[0].DocumentRef() {
		t.Fatal(err)
	}
	// Removing the identity invalidates the frozen index even before reading bodies.
	decoded.Entries[0].DocIdentity = DocumentIdentityLegacy
	if ValidateStagingIndex(decoded) == nil {
		t.Fatal("stripped identity accepted")
	}
	index.Version = 1
	if !errors.Is(ValidateStagingIndex(index), ErrStagingVersion) {
		t.Fatal("root accepted by legacy version")
	}
}

func TestRootContextDiffVerifiesBothIdentitiesAndContext(t *testing.T) {
	cir := identityTestCIR(t, "one")
	before, _, _ := identityTestRoot(t, cir)
	cir.Events = append(cir.Events, Event{Seq: 1, Kind: EventMessage, Role: "assistant", Blocks: []ContentBlock{{Type: "text", Text: "two"}}})
	after, _, _ := identityTestRoot(t, cir)
	out, err := CompareContextDocuments(context.Background(), &before, after)
	if err != nil || out.State != "extended" || out.BeforeIdentity != DocumentIdentityRootV1 || out.AfterIdentity != DocumentIdentityRootV1 {
		t.Fatal(out, err)
	}
	legacy := before
	legacy.Identity = DocumentIdentityLegacy
	raw, _ := CanonicalBytes(legacy.CIR)
	legacy.Hash = HashContent(raw)
	out, err = CompareContextDocuments(context.Background(), &legacy, after)
	if err != nil || out.BeforeIdentity != "" || out.State != "extended" {
		t.Fatal(out, err)
	}
	stripped := after
	stripped.Identity = ""
	if _, err := CompareContextDocuments(context.Background(), &before, stripped); err == nil {
		t.Fatal("hash-only root accepted")
	}
	corrupt := after
	corrupt.CIR.Events = append([]Event{}, after.CIR.Events...)
	corrupt.CIR.Events[0].Role = "assistant"
	if _, err := CompareContextDocuments(context.Background(), &before, corrupt); err == nil {
		t.Fatal("corrupt body accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CompareContextDocuments(ctx, &before, after); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
