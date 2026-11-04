package domain

import "context"

type DocRangeDirectoryAccumulator struct {
	Count  int             `json:"count"`
	Levels [][]ContentHash `json:"levels"`
}

func (accumulator DocRangeDirectoryAccumulator) Validate() error {
	if accumulator.Count < 0 || accumulator.Count > MaxDocRangePages {
		return ErrIntegrity
	}
	remaining := accumulator.Count
	for _, level := range accumulator.Levels {
		if remaining == 0 || len(level) != remaining%DocRangeDirectoryFanout {
			return ErrIntegrity
		}
		for _, hash := range level {
			if ValidateContentHash(hash) != nil {
				return ErrIntegrity
			}
		}
		remaining /= DocRangeDirectoryFanout
	}
	if remaining != 0 {
		return ErrIntegrity
	}
	return nil
}

func (accumulator DocRangeDirectoryAccumulator) clone() DocRangeDirectoryAccumulator {
	copy := DocRangeDirectoryAccumulator{Count: accumulator.Count, Levels: make([][]ContentHash, len(accumulator.Levels))}
	for level, hashes := range accumulator.Levels {
		copy.Levels[level] = append([]ContentHash(nil), hashes...)
	}
	return copy
}

func (accumulator *DocRangeDirectoryAccumulator) appendLevel(ctx context.Context, level int, hash ContentHash, objects *[]DocRangeObject) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if level == len(accumulator.Levels) {
		accumulator.Levels = append(accumulator.Levels, nil)
	}
	accumulator.Levels[level] = append(accumulator.Levels[level], hash)
	if len(accumulator.Levels[level]) < DocRangeDirectoryFanout {
		return nil
	}
	parent, err := accumulator.flush(ctx, level, objects)
	if err != nil {
		return err
	}
	return accumulator.appendLevel(ctx, level+1, parent, objects)
}

func (accumulator *DocRangeDirectoryAccumulator) flush(ctx context.Context, level int, objects *[]DocRangeObject) (ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	span := DocRangeDirectoryFanout
	for position := 0; position < level; position++ {
		span *= DocRangeDirectoryFanout
	}
	directory := docRangeDirectory{Level: level, Start: (accumulator.Count - 1) / span * span, Hashes: accumulator.Levels[level]}
	raw, err := canonicalJSON(directory)
	if err != nil || len(raw) > MaxDocRangeDirectoryBytes {
		return "", ErrIntegrity
	}
	hash := HashContent(raw)
	*objects = append(*objects, DocRangeObject{Hash: hash, Bytes: raw})
	accumulator.Levels[level] = nil
	return hash, nil
}

func (accumulator DocRangeDirectoryAccumulator) Append(ctx context.Context, hash ContentHash) (result DocRangeDirectoryAccumulator, produced []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, produced, failure = DocRangeDirectoryAccumulator{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocRangeDirectoryAccumulator{}, nil, err
	}
	if accumulator.Validate() != nil || ValidateContentHash(hash) != nil || accumulator.Count == MaxDocRangePages {
		return DocRangeDirectoryAccumulator{}, nil, ErrIntegrity
	}
	next := accumulator.clone()
	next.Count++
	var objects []DocRangeObject
	if err := next.appendLevel(ctx, 0, hash, &objects); err != nil {
		return DocRangeDirectoryAccumulator{}, nil, err
	}
	return next, objects, nil
}

func (accumulator DocRangeDirectoryAccumulator) Finish(ctx context.Context) (result DocRangeDirectoryRoot, produced []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, produced, failure = DocRangeDirectoryRoot{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocRangeDirectoryRoot{}, nil, err
	}
	if accumulator.Validate() != nil {
		return DocRangeDirectoryRoot{}, nil, ErrIntegrity
	}
	root := DocRangeDirectoryRoot{Count: accumulator.Count}
	if root.Count == 0 {
		return root, nil, nil
	}
	next := accumulator.clone()
	var objects []DocRangeObject
	for level := 0; level < len(next.Levels); level++ {
		if len(next.Levels[level]) == 0 {
			continue
		}
		if level > 0 && level == len(next.Levels)-1 && len(next.Levels[level]) == 1 {
			root.Hash = next.Levels[level][0]
			return root, objects, ctx.Err()
		}
		hash, err := next.flush(ctx, level, &objects)
		if err != nil {
			return DocRangeDirectoryRoot{}, nil, err
		}
		if err := next.appendLevel(ctx, level+1, hash, &objects); err != nil {
			return DocRangeDirectoryRoot{}, nil, err
		}
	}
	return DocRangeDirectoryRoot{}, nil, ErrIntegrity
}
