package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"unsafe"
)

var errLegacyCanonicalNumber = fmt.Errorf("%w: invalid legacy canonical number", ErrValidation)

type legacyCanonicalNumberIdentity struct {
	start *byte
	size  int
}

type legacyCanonicalNumberValidation struct {
	pending  legacyCanonicalPages[string]
	proven   legacyCanonicalNumberRegistry
	next     int
	position int
	mode     legacyJSONValueMode
}

func legacyCanonicalNumberKey(text string) legacyCanonicalNumberIdentity {
	return legacyCanonicalNumberIdentity{start: unsafe.StringData(text), size: len(text)}
}

func (numbers *legacyCanonicalNumberValidation) prepareBytes(text string) (int, error) {
	work, _, err := numbers.admission(text)
	return work, err
}

func (numbers *legacyCanonicalNumberValidation) admission(text string) (int, bool, error) {
	registryBytes, exists, err := numbers.proven.plan(legacyCanonicalNumberKey(text))
	if err != nil || exists {
		return registryBytes, exists, err
	}
	pendingBytes, err := numbers.pending.appendBytes()
	if err != nil {
		return 0, false, err
	}
	if registryBytes > LegacyCanonicalEncodingBytes-pendingBytes {
		return 0, false, ErrIntegrity
	}
	return registryBytes + pendingBytes, false, nil
}

func (numbers *legacyCanonicalNumberValidation) prepare(text string) error {
	_, exists, err := numbers.admission(text)
	if err != nil || exists {
		return err
	}
	if err := numbers.pending.append(text); err != nil {
		return err
	}
	numbers.proven.insertIndexed(legacyCanonicalNumberKey(text), numbers.pending.length()-1)
	return nil
}

func (numbers *legacyCanonicalNumberValidation) index(text string) (int, error) {
	if numbers == nil || !numbers.pending.valid() || numbers.pending.length() != numbers.proven.length() {
		return 0, ErrIntegrity
	}
	entry, err := numbers.proven.lookup(legacyCanonicalNumberKey(text))
	if err != nil {
		return 0, err
	}
	pending := numbers.pending.at(entry.ordinal)
	if pending == nil || legacyCanonicalNumberKey(*pending) != entry.identity {
		return 0, ErrIntegrity
	}
	return entry.ordinal, nil
}

func (encoding *LegacyCanonicalEncoding) prepareNumber(ctx context.Context, text string) error {
	if err := encoding.numbers.prepare(text); err != nil {
		return err
	}
	if !encoding.eagerNumbers {
		return nil
	}
	for {
		complete, err := encoding.numbers.step(ctx)
		if err != nil {
			if err == errLegacyCanonicalNumber {
				_, nativeErr := json.Marshal(json.Number(text))
				err = nativeErr
				if err == nil {
					err = ErrIntegrity
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if complete {
			return nil
		}
	}
}

func (numbers *legacyCanonicalNumberValidation) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	work := 0
	for actions := 0; numbers.next < numbers.pending.length() && actions < legacyCanonicalEncodingActions; actions++ {
		pending := numbers.pending.at(numbers.next)
		if pending == nil {
			return false, ErrIntegrity
		}
		text := *pending
		for numbers.position < len(text) && work < LegacyCanonicalEncodingBytes {
			if work%canonicalCheckBytes == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			next, valid := legacyJSONNumberNext(numbers.mode, text[numbers.position])
			if !valid {
				return false, errLegacyCanonicalNumber
			}
			numbers.mode = next
			numbers.position++
			work++
		}
		if numbers.position != len(text) {
			break
		}
		if text != "" && !legacyJSONNumberFinal(numbers.mode) {
			return false, errLegacyCanonicalNumber
		}
		if legacyCanonicalNumberProofBytes > LegacyCanonicalEncodingBytes-work {
			break
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := numbers.proven.prove(legacyCanonicalNumberKey(text)); err != nil {
			return false, err
		}
		work += legacyCanonicalNumberProofBytes
		numbers.next++
		numbers.position, numbers.mode = 0, legacyJSONValueIdle
		if work == LegacyCanonicalEncodingBytes {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return numbers.next == numbers.pending.length(), nil
}

func (numbers *legacyCanonicalNumberValidation) valid(text string) bool {
	proven, _ := numbers.proven.get(legacyCanonicalNumberKey(text))
	return proven
}

func (numbers *legacyCanonicalNumberValidation) retained(heap *docRangeHeaderHeapEstimate) {
	numbers.pending.retained(heap)
	numbers.proven.retained(heap)
}
