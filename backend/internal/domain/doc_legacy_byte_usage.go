package domain

type LegacyDerivedByteStorageUsage struct {
	Sources           int64
	CompleteSources   int64
	Objects           int64
	Parts             int64
	PayloadBytes      int64
	HeaderBytes       int64
	HeaderParts       int64
	CheckpointBytes   int64
	CheckpointObjects int64
	SpecBytes         int64
	TableBytes        int64
	IndexBytes        int64
}

func (usage LegacyDerivedByteStorageUsage) Validate() error {
	if usage.Sources < 0 || usage.CompleteSources < 0 || usage.Objects < 0 || usage.Parts < 0 ||
		usage.PayloadBytes < 0 || usage.HeaderBytes < 0 || usage.HeaderParts < 0 || usage.SpecBytes < 0 ||
		usage.CheckpointBytes < 0 || usage.CheckpointObjects < 0 || usage.TableBytes < 0 || usage.IndexBytes < 0 {
		return ErrValidation
	}
	if usage.CompleteSources > usage.Sources {
		return ErrValidation
	}
	return nil
}
