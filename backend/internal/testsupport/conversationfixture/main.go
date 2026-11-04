//go:build conversationfixture

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var document domain.CIRDocument
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("fixture must contain exactly one document: %v", err)
	}
	canonical, err := domain.CanonicalBytes(document)
	if err != nil {
		return err
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		return err
	}
	root, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		return err
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return err
	}
	chunks := make([]inbound.ChunkObject, 0, len(bodies))
	for hash, data := range bodies {
		chunks = append(chunks, inbound.ChunkObject{Hash: hash, Data: data})
	}
	slices.SortFunc(chunks, func(left, right inbound.ChunkObject) int {
		if left.Hash < right.Hash {
			return -1
		}
		if left.Hash > right.Hash {
			return 1
		}
		return 0
	})
	return json.NewEncoder(os.Stdout).Encode(struct {
		Legacy domain.SessionDoc             `json:"legacy"`
		Root   domain.DocumentRepresentation `json:"root"`
		Chunks []inbound.ChunkObject         `json:"chunks"`
	}{domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: document},
		domain.DocumentRepresentation{Hash: root, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, chunks})
}
