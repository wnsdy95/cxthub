package outbound

type TransientSpoolBudgetUsage struct {
	LimitBytes int64
	UsedBytes  int64
	PeakBytes  int64
	OpenFiles  int
}

type TransientSpoolBudgetStore interface {
	ConfigureTransientSpoolBudget(int64) (func() error, error)
	TransientSpoolBudgetUsage() TransientSpoolBudgetUsage
}
