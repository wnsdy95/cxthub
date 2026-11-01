package domain

import (
	"context"
	"encoding/binary"
)

const (
	legacyJSONCheckpointVersion = 1
	legacyJSONCheckpointHeader  = 10
	legacyJSONCheckpointKey     = 4
)

func (scanner *legacyJSONValueScanner) checkpoint(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanner == nil {
		return nil, ErrIntegrity
	}
	if scanner.invalid || scanner.complete {
		raw := make([]byte, legacyJSONCheckpointHeader)
		raw[0] = legacyJSONCheckpointVersion
		raw[1] = 1
		if scanner.invalid {
			raw[1] = 2
		}
		return raw, nil
	}
	if err := scanner.validateCheckpoint(); err != nil {
		return nil, err
	}
	raw := make([]byte, legacyJSONCheckpointHeader+scanner.depth)
	raw[0] = legacyJSONCheckpointVersion
	if scanner.key {
		raw[1] = legacyJSONCheckpointKey
	}
	raw[2] = byte(scanner.mode)
	switch scanner.mode {
	case legacyJSONValueLiteral:
		switch scanner.literal {
		case "true":
			raw[3] = 1
		case "false":
			raw[3] = 2
		case "null":
			raw[3] = 3
		}
		raw[4] = scanner.literalNext
	case legacyJSONValueUnicode:
		raw[5] = scanner.unicodeLeft
	}
	binary.BigEndian.PutUint16(raw[6:8], uint16(scanner.parents))
	binary.BigEndian.PutUint16(raw[8:10], uint16(scanner.depth))
	for index := range scanner.depth {
		raw[legacyJSONCheckpointHeader+index] = byte(scanner.stack[index])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func restoreLegacyJSONValueScanner(ctx context.Context, raw []byte) (legacyJSONValueScanner, error) {
	var scanner legacyJSONValueScanner
	if err := ctx.Err(); err != nil {
		return scanner, err
	}
	if len(raw) < legacyJSONCheckpointHeader || raw[0] != legacyJSONCheckpointVersion {
		return scanner, ErrIntegrity
	}
	status := raw[1]
	if status == 1 || status == 2 {
		if len(raw) != legacyJSONCheckpointHeader {
			return scanner, ErrIntegrity
		}
		for _, current := range raw[2:] {
			if current != 0 {
				return scanner, ErrIntegrity
			}
		}
		scanner.complete = status == 1
		scanner.invalid = status == 2
		return scanner, nil
	}
	if status != 0 && status != legacyJSONCheckpointKey {
		return scanner, ErrIntegrity
	}
	scanner.parents = int(binary.BigEndian.Uint16(raw[6:8]))
	scanner.depth = int(binary.BigEndian.Uint16(raw[8:10]))
	if scanner.parents > len(scanner.stack) || scanner.depth > len(scanner.stack)-scanner.parents ||
		len(raw) != legacyJSONCheckpointHeader+scanner.depth {
		return legacyJSONValueScanner{}, ErrIntegrity
	}
	scanner.mode = legacyJSONValueMode(raw[2])
	scanner.key = status == legacyJSONCheckpointKey
	if scanner.mode == legacyJSONValueLiteral {
		switch raw[3] {
		case 1:
			scanner.literal = "true"
		case 2:
			scanner.literal = "false"
		case 3:
			scanner.literal = "null"
		default:
			return legacyJSONValueScanner{}, ErrIntegrity
		}
		scanner.literalNext = raw[4]
	} else if raw[3] != 0 || raw[4] != 0 {
		return legacyJSONValueScanner{}, ErrIntegrity
	}
	if scanner.mode == legacyJSONValueUnicode {
		scanner.unicodeLeft = raw[5]
	} else if raw[5] != 0 {
		return legacyJSONValueScanner{}, ErrIntegrity
	}
	for index := range scanner.depth {
		scanner.stack[index] = legacyJSONValueFrame(raw[legacyJSONCheckpointHeader+index])
	}
	if err := scanner.validateCheckpoint(); err != nil {
		return legacyJSONValueScanner{}, err
	}
	if err := ctx.Err(); err != nil {
		return legacyJSONValueScanner{}, err
	}
	return scanner, nil
}

func (scanner *legacyJSONValueScanner) validateCheckpoint() error {
	if scanner.parents < 0 || scanner.parents > len(scanner.stack) || scanner.depth < 0 ||
		scanner.depth > len(scanner.stack)-scanner.parents || scanner.mode > legacyJSONValueExponentDigits {
		return ErrIntegrity
	}
	if scanner.mode == legacyJSONValueLiteral {
		if scanner.literal != "true" && scanner.literal != "false" && scanner.literal != "null" ||
			scanner.literalNext == 0 || int(scanner.literalNext) >= len(scanner.literal) {
			return ErrIntegrity
		}
	}
	if scanner.mode == legacyJSONValueUnicode && (scanner.unicodeLeft == 0 || scanner.unicodeLeft > 4) {
		return ErrIntegrity
	}
	stringMode := scanner.mode == legacyJSONValueString || scanner.mode == legacyJSONValueEscape ||
		scanner.mode == legacyJSONValueUnicode
	if scanner.key && (!stringMode || scanner.depth == 0) {
		return ErrIntegrity
	}
	for index := range scanner.depth {
		frame := scanner.stack[index]
		if frame > legacyJSONValueArrayAfter {
			return ErrIntegrity
		}
		if index < scanner.depth-1 && !legacyJSONCheckpointValueFrame(frame) {
			return ErrIntegrity
		}
	}
	if scanner.depth > 0 && scanner.mode != legacyJSONValueIdle {
		frame := scanner.stack[scanner.depth-1]
		if scanner.key {
			if frame != legacyJSONValueObjectFirst && frame != legacyJSONValueObjectKey {
				return ErrIntegrity
			}
		} else if !legacyJSONCheckpointValueFrame(frame) {
			return ErrIntegrity
		}
	}
	return nil
}

func legacyJSONCheckpointValueFrame(frame legacyJSONValueFrame) bool {
	return frame == legacyJSONValueObjectValue || frame == legacyJSONValueArrayFirst ||
		frame == legacyJSONValueArrayValue
}
