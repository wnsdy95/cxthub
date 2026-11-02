package domain

type legacyCanonicalKeyField struct {
	key       string
	value     any
	childKeys int
}

type legacyCanonicalKeyAccess interface {
	length() int
	at(int) *legacyCanonicalKeyField
}

type legacyCanonicalKeyOrderPhase uint8

const (
	legacyCanonicalKeyOrderInvalid legacyCanonicalKeyOrderPhase = iota
	legacyCanonicalKeyOrderDone
	legacyCanonicalKeyOrderBuild
	legacyCanonicalKeyOrderSift
	legacyCanonicalKeyOrderCompareChildren
	legacyCanonicalKeyOrderParent
	legacyCanonicalKeyOrderCompareParent
	legacyCanonicalKeyOrderSwap
	legacyCanonicalKeyOrderExtract
)

type legacyCanonicalKeyOrder struct {
	phase         legacyCanonicalKeyOrderPhase
	count         int
	build         int
	limit         int
	root          int
	candidate     int
	compareLeft   int
	compareRight  int
	compareOffset int
	work          uint64
}

func newLegacyCanonicalKeyOrder(count int, sorted bool) legacyCanonicalKeyOrder {
	order := legacyCanonicalKeyOrder{
		count:        count,
		limit:        count,
		compareLeft:  -1,
		compareRight: -1,
	}
	if count < 0 {
		return order
	}
	if sorted || count < 2 {
		order.phase = legacyCanonicalKeyOrderDone
		return order
	}
	order.phase = legacyCanonicalKeyOrderBuild
	order.build = count / 2
	return order
}

func (order *legacyCanonicalKeyOrder) valid(fields legacyCanonicalKeyAccess) bool {
	if order == nil || order.count < 0 || fields == nil && order.count != 0 ||
		fields != nil && fields.length() != order.count ||
		order.phase <= legacyCanonicalKeyOrderInvalid || order.phase > legacyCanonicalKeyOrderExtract ||
		order.build < 0 || order.build > order.count/2 || order.limit < 0 || order.limit > order.count ||
		order.compareOffset < 0 {
		return false
	}
	if order.count > 0 && (fields.at(0) == nil || fields.at(order.count-1) == nil) {
		return false
	}
	comparing := order.phase == legacyCanonicalKeyOrderCompareChildren || order.phase == legacyCanonicalKeyOrderCompareParent
	if !comparing && (order.compareLeft != -1 || order.compareRight != -1 || order.compareOffset != 0) {
		return false
	}
	if order.phase == legacyCanonicalKeyOrderDone {
		return order.build == 0 && (order.limit == order.count || order.limit == 1)
	}
	if order.count < 2 || order.limit < 2 || order.root < 0 || order.root >= order.limit ||
		order.limit < order.count && order.build != 0 ||
		order.phase != legacyCanonicalKeyOrderBuild && order.root < order.build {
		return false
	}
	switch order.phase {
	case legacyCanonicalKeyOrderBuild:
		return order.limit == order.count
	case legacyCanonicalKeyOrderExtract:
		return order.build == 0
	case legacyCanonicalKeyOrderSift:
		return true
	}
	if order.root >= order.limit/2 {
		return false
	}
	left := order.root*2 + 1
	if order.candidate < left || order.candidate > left+1 || order.candidate >= order.limit {
		return false
	}
	if !comparing {
		return true
	}
	if order.phase == legacyCanonicalKeyOrderCompareChildren {
		if order.candidate != left || left+1 >= order.limit || order.compareLeft != left+1 || order.compareRight != left {
			return false
		}
	} else if order.compareLeft != order.candidate || order.compareRight != order.root {
		return false
	}
	leftField, rightField := fields.at(order.compareLeft), fields.at(order.compareRight)
	return leftField != nil && rightField != nil && order.compareOffset <= min(len(leftField.key), len(rightField.key))
}

func (order *legacyCanonicalKeyOrder) finishSift() {
	if order.limit == order.count {
		order.phase = legacyCanonicalKeyOrderBuild
	} else {
		order.phase = legacyCanonicalKeyOrderExtract
	}
}

func (order *legacyCanonicalKeyOrder) finishComparison(result int) {
	if order.phase == legacyCanonicalKeyOrderCompareChildren {
		if result > 0 {
			order.candidate = order.compareLeft
		}
		order.phase = legacyCanonicalKeyOrderParent
	} else if result > 0 {
		order.phase = legacyCanonicalKeyOrderSwap
	} else {
		order.finishSift()
	}
	order.compareLeft, order.compareRight, order.compareOffset = -1, -1, 0
}

func (order *legacyCanonicalKeyOrder) advance(fields legacyCanonicalKeyAccess, remaining int) (used int, complete bool, yield bool, err error) {
	if remaining < 0 || !order.valid(fields) {
		return 0, false, false, ErrIntegrity
	}
	if order.phase == legacyCanonicalKeyOrderDone {
		return 0, true, false, nil
	}
	if order.work == ^uint64(0) {
		return 0, false, false, ErrIntegrity
	}
	switch order.phase {
	case legacyCanonicalKeyOrderBuild:
		if order.build == 0 {
			order.phase = legacyCanonicalKeyOrderExtract
		} else {
			order.build--
			order.root = order.build
			order.phase = legacyCanonicalKeyOrderSift
		}
	case legacyCanonicalKeyOrderSift:
		if order.root >= order.limit/2 {
			order.finishSift()
		} else {
			order.candidate = order.root*2 + 1
			if order.candidate+1 < order.limit {
				order.compareLeft, order.compareRight = order.candidate+1, order.candidate
				order.phase = legacyCanonicalKeyOrderCompareChildren
			} else {
				order.phase = legacyCanonicalKeyOrderParent
			}
		}
	case legacyCanonicalKeyOrderParent:
		order.compareLeft, order.compareRight = order.candidate, order.root
		order.phase = legacyCanonicalKeyOrderCompareParent
	case legacyCanonicalKeyOrderCompareChildren, legacyCanonicalKeyOrderCompareParent:
		leftField, rightField := fields.at(order.compareLeft), fields.at(order.compareRight)
		if leftField == nil || rightField == nil {
			return 0, false, false, ErrIntegrity
		}
		leftKey, rightKey := leftField.key, rightField.key
		common := min(len(leftKey), len(rightKey))
		if order.compareOffset < common {
			if remaining < 2 {
				return 0, false, true, nil
			}
			width := min(remaining/2, common-order.compareOffset)
			for scanned := 0; scanned < width; scanned++ {
				leftByte, rightByte := leftKey[order.compareOffset+scanned], rightKey[order.compareOffset+scanned]
				if leftByte < rightByte {
					order.finishComparison(-1)
					order.work++
					return 2 * (scanned + 1), false, false, nil
				}
				if leftByte > rightByte {
					order.finishComparison(1)
					order.work++
					return 2 * (scanned + 1), false, false, nil
				}
			}
			used = 2 * width
			order.compareOffset += width
			if order.compareOffset < common {
				order.work++
				return used, false, true, nil
			}
		}
		result := 0
		if len(leftKey) < len(rightKey) {
			result = -1
		} else if len(leftKey) > len(rightKey) {
			result = 1
		}
		order.finishComparison(result)
	case legacyCanonicalKeyOrderSwap:
		rootField, candidateField := fields.at(order.root), fields.at(order.candidate)
		if rootField == nil || candidateField == nil {
			return 0, false, false, ErrIntegrity
		}
		*rootField, *candidateField = *candidateField, *rootField
		order.root = order.candidate
		order.phase = legacyCanonicalKeyOrderSift
	case legacyCanonicalKeyOrderExtract:
		rootField, lastField := fields.at(0), fields.at(order.limit-1)
		if rootField == nil || lastField == nil {
			return 0, false, false, ErrIntegrity
		}
		order.limit--
		*rootField, *lastField = *lastField, *rootField
		order.root = 0
		if order.limit == 1 {
			order.phase = legacyCanonicalKeyOrderDone
		} else {
			order.phase = legacyCanonicalKeyOrderSift
		}
	}
	order.work++
	return used, order.phase == legacyCanonicalKeyOrderDone, false, nil
}
