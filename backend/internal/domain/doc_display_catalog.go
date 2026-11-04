package domain

import (
	"bytes"
	"encoding/json"
)

const DocDisplayCatalogCensusBatch = MaxDocEventDisplayStepBytes / MaxDocDisplayPageBytes

type DocDisplayCatalogUse struct {
	Source     ContentHash
	Generation string
	Epoch      string
}

func (use DocDisplayCatalogUse) Validate() error {
	if ValidateContentHash(use.Source) != nil || ValidateDocDisplayCatalogEpoch(use.Generation) != nil || ValidateDocDisplayCatalogEpoch(use.Epoch) != nil {
		return ErrIntegrity
	}
	return nil
}

func ValidateDocDisplayCatalogEpoch(epoch string) error {
	if !catalogEpochValid(epoch) || epoch == "00000000-0000-0000-0000-000000000000" {
		return ErrIntegrity
	}
	return nil
}

type DocDisplayCatalogSource struct {
	EventHash ContentHash
	Profile   string
	Hash      ContentHash
	Bytes     []byte
}

func (source DocDisplayCatalogSource) Validate() error {
	if len(source.Bytes) == 0 || len(source.Bytes) > MaxDocDisplaySourceBytes || ValidateContentHash(source.Hash) != nil || HashContent(source.Bytes) != source.Hash {
		return ErrIntegrity
	}
	switch source.Profile {
	case DocDisplayProjectionProfile:
		var value DocEventDisplaySource
		if json.Unmarshal(source.Bytes, &value) != nil || value.EventHash != source.EventHash || value.Profile != source.Profile {
			return ErrIntegrity
		}
		raw, err := value.Encode()
		if err != nil || !bytes.Equal(raw, source.Bytes) {
			return ErrIntegrity
		}
	case DocDisplayStringProfile:
		value, err := DecodeDocDisplayStringSource(source.Hash, source.Bytes)
		if err != nil || value.EventHash != source.EventHash || value.Profile != source.Profile {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

type DocDisplayCatalogReceipt struct {
	EventHash       ContentHash
	EventBytes      int
	Profile         string
	State           string
	WorkerProtocol  int
	Discovering     bool
	Source          ContentHash
	CheckpointHash  ContentHash
	Checkpoint      []byte
	PreparationHash ContentHash
	Preparation     []byte
}

func (receipt DocDisplayCatalogReceipt) CatalogProtection(source DocDisplayCatalogSource) string {
	if receipt.State != "completed" {
		return "resumable_job"
	}
	if source.Validate() != nil || receipt.Source != source.Hash || receipt.EventHash != source.EventHash || receipt.Profile != source.Profile || receipt.Discovering {
		return "unverified_completion"
	}
	switch receipt.Profile {
	case DocDisplayProjectionProfile:
		if receipt.WorkerProtocol != 5 || receipt.CheckpointHash != "" || len(receipt.Checkpoint) != 0 {
			return "unverified_completion"
		}
		state, err := DecodeDocEventDisplayPreparation(receipt.PreparationHash, receipt.Preparation)
		if err != nil || state.Phase != "completed" || state.Source.EventHash != receipt.EventHash || state.Source.EventBytes != receipt.EventBytes {
			return "unverified_completion"
		}
		raw, err := state.Source.Encode()
		if err != nil || !bytes.Equal(raw, source.Bytes) {
			return "unverified_completion"
		}
	case DocDisplayStringProfile:
		if receipt.WorkerProtocol != 2 || receipt.PreparationHash != "" || len(receipt.Preparation) != 0 {
			return "unverified_completion"
		}
		value, err := DecodeDocDisplayStringSource(source.Hash, source.Bytes)
		if err != nil || value.EventBytes != receipt.EventBytes {
			return "unverified_completion"
		}
		state, err := DecodeDocDisplayStringState(value, receipt.CheckpointHash, receipt.Checkpoint)
		if err != nil || !state.Done {
			return "unverified_completion"
		}
	default:
		return "unverified_completion"
	}
	return ""
}
