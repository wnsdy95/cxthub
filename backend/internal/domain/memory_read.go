package domain

const (
	MemoryReadVersion        = 1
	MemoryReadRules          = 1
	MaxMemoryReadHeaderBytes = 4 << 20
	MaxMemoryReadBytes       = 64 << 20
	MaxMemoryReadItems       = 16384
	MaxMemoryReadChunks      = 1024
	MaxMemoryReadChunkItems  = 64
	MaxMemoryReadChunkBytes  = (256 << 10) + 2
)

type MemoryReadDependency struct {
	Hash      ContentHash `json:"hash"`
	Available bool        `json:"available"`
}

type MemoryReadChunkRef struct {
	Hash  ContentHash `json:"hash"`
	Start int         `json:"start"`
	Count int         `json:"count"`
	Bytes int         `json:"bytes"`
}

type MemoryReadHeader struct {
	Version      int                              `json:"version"`
	Rules        int                              `json:"rules"`
	CaptureID    ContentHash                      `json:"capture_id"`
	Source       RepositoryMemorySourceGeneration `json:"source"`
	Origin       string                           `json:"origin"`
	View         EffectiveMemoryPage              `json:"view"`
	Dependencies []MemoryReadDependency           `json:"dependencies"`
	Chunks       []MemoryReadChunkRef             `json:"chunks"`
}

type MemoryReadChunk struct {
	Hash  ContentHash
	Bytes []byte
}

type MemoryReadBundle struct {
	ID     ContentHash
	Header MemoryReadHeader
	Chunks []MemoryReadChunk
}
