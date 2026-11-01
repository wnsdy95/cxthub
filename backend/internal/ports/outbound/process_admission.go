package outbound

import "context"

const BoundedDocumentsProfile = "bounded-documents-v1"
const BoundedWorkersProfile = "bounded-workers-v1"
const BoundedDocumentsControlPrefix = "cxthub/bd1/"

type ProcessRole string

const (
	ProcessAPI    ProcessRole = "api"
	ProcessMCP    ProcessRole = "mcp"
	ProcessWorker ProcessRole = "worker"
)

type ProcessAdmissionConfig struct {
	Role                ProcessRole
	DataPoolMax         int32
	CoordinationPoolMax int32
	DBConnectionBudget  int32
	ReservedConnections int32
	BudgetFingerprint   string
}

type ProcessAdmissionStore interface {
	OpenProcessAdmission(context.Context, ProcessAdmissionConfig) (ProcessAdmission, error)
}

type ProcessAdmission interface {
	Acquire(context.Context, bool, bool) (ProcessAdmissionPermit, error)
	Context() context.Context
	Ready() bool
	Err() error
	Close() error
}

type ProcessAdmissionPermit interface {
	Upgrade(context.Context, bool) error
	Park(context.Context) error
	Resume(context.Context) error
	Release() error
}
