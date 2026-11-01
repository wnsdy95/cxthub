package domain

import "context"

const legacyNativeCheckpointVersion = 1
const legacyNativeCheckpointPageItems = 32
const legacyNativeCheckpointTreeHeight = 8
const legacyNativeCheckpointNodes = uint64(MaxFinalizedDocBytes)

type legacyNativeCheckpointRef struct {
	Hash  ContentHash `json:"h"`
	Bytes int         `json:"n"`
}

type legacyNativeCheckpointIO struct {
	Put  func(context.Context, ContentHash, []byte) error
	Read func(context.Context, ContentHash, int) ([]byte, error)
}

type legacyNativeCheckpointTree struct {
	Ref    legacyNativeCheckpointRef `json:"r"`
	Count  uint64                    `json:"n"`
	Height uint8                     `json:"h"`
}

type legacyNativeCheckpointText struct {
	Bytes int                        `json:"n"`
	Tree  legacyNativeCheckpointTree `json:"t"`
}

type legacyNativeCheckpointScalar struct {
	State legacyNativeScalarCheckpoint `json:"s"`
	Text  legacyNativeCheckpointText   `json:"t"`
}

type legacyNativeCheckpointValue struct {
	Wire []byte                     `json:"v"`
	Body legacyNativeCheckpointRef  `json:"b"`
	Text legacyNativeCheckpointText `json:"t"`
}

type legacyNativeCheckpointMember struct {
	Key   legacyNativeCheckpointValue `json:"k"`
	Value legacyNativeCheckpointValue `json:"v"`
}

type legacyNativeCheckpointOwner struct {
	Version     uint8                            `json:"v"`
	Identity    uint64                           `json:"i"`
	Kind        uint8                            `json:"k"`
	Length      uint64                           `json:"n"`
	Capacity    uint64                           `json:"c"`
	Maximum     uint64                           `json:"m"`
	Revision    uint64                           `json:"r"`
	Members     legacyNativeCheckpointTree       `json:"e"`
	Dirty       legacyNativeCheckpointTree       `json:"d"`
	FullBacking bool                             `json:"f,omitempty"`
	Visible     uint64                           `json:"l,omitempty"`
	Allocation  *legacyNativeCheckpointArrayView `json:"a,omitempty"`
}

func (record legacyNativeCheckpointOwner) logicalLength() uint64 {
	if record.FullBacking {
		return record.Visible
	}
	return record.Length
}

func (record legacyNativeCheckpointOwner) validateBacking() error {
	if record.Allocation != nil && (record.Kind != legacyNativeCheckpointArray || record.Capacity == 0 || record.Allocation.Offset > legacyNativeCheckpointNodes || !record.Allocation.Backing.Body.valid() || len(record.Allocation.Backing.Wire) == 0) {
		return ErrIntegrity
	}
	if record.FullBacking {
		if record.Kind != legacyNativeCheckpointArray || record.Length != record.Capacity || record.Visible >= record.Length {
			return ErrIntegrity
		}
	} else if record.Visible != 0 {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeAnyRestored struct {
	GraphBytes int64
	Parser     *legacyNativeAny
	Scanner    legacyJSONValueScanner
	Visible    legacyNativeAnyValue
	Source     LegacyByteSource
}
