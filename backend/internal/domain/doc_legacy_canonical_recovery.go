package domain

import "context"

type LegacyCanonicalDocumentSource struct {
	EnvelopeBytes int
	StreamBytes   int
	ReadEnvelope  func(context.Context, int, int) ([]byte, error)
	ReadStream    func(context.Context, int, int) ([]byte, error)
}

func (source LegacyCanonicalDocumentSource) Size() (int, error) {
	if source.EnvelopeBytes < 2 || source.StreamBytes < 0 || source.ReadEnvelope == nil || source.ReadStream == nil {
		return 0, ErrIntegrity
	}
	if source.StreamBytes > MaxFinalizedDocBytes-canonicalFrameBytes || source.EnvelopeBytes > MaxFinalizedDocBytes-canonicalFrameBytes-source.StreamBytes {
		return 0, ErrDocumentResourceLimit
	}
	return canonicalFrameBytes + source.EnvelopeBytes + source.StreamBytes, nil
}

func (source LegacyCanonicalDocumentSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	extent, err := source.Size()
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > extent || length > extent-offset {
		return nil, ErrIntegrity
	}
	if length > ConversationManifestChunkBytes {
		return nil, ErrDocumentResourceLimit
	}
	sizes := [5]int{len(canonicalDocPrefix), source.EnvelopeBytes, len(canonicalDocMiddle), source.StreamBytes, len(canonicalDocSuffix)}
	literals := [5]string{canonicalDocPrefix, "", canonicalDocMiddle, "", canonicalDocSuffix}
	body := make([]byte, length)
	position, copied := 0, 0
	for segment, size := range sizes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		from, to := max(offset, position), min(offset+length, position+size)
		if from < to {
			var payload []byte
			var err error
			switch segment {
			case 1:
				payload, err = source.ReadEnvelope(ctx, from-position, to-from)
			case 3:
				payload, err = source.ReadStream(ctx, from-position, to-from)
			default:
				copied += copy(body[copied:], literals[segment][from-position:to-position])
				position += size
				continue
			}
			if canceled := ctx.Err(); canceled != nil {
				return nil, canceled
			}
			if err != nil {
				return nil, err
			}
			if len(payload) != to-from {
				return nil, ErrPreparedDocRangeObject
			}
			copied += copy(body[copied:], payload)
		}
		position += size
	}
	if copied != length {
		return nil, ErrPreparedDocRangeObject
	}
	return body, ctx.Err()
}
