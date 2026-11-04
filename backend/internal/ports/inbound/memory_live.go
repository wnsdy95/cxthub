package inbound

type EffectiveMemoryReader interface {
	MemoryReadScope
	EffectiveMemoryQuery
}

type BranchMemoryReader interface {
	MemoryReadScope
	BranchMemoryQuery
}

type MemoryPositionReader interface {
	MemoryReadScope
	MemoryPositionQuery
}

type LiveMemoryQueries interface {
	EffectiveMemoryReader
	BranchMemoryQuery
	MemoryPositionQuery
}
