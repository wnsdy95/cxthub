package domain

const MaxPreparedMemoryCursorBytes = 4096

type MemoryReadPending struct {
	Cursor string
}

func (pending *MemoryReadPending) Error() string { return ErrMemoryReadNotReady.Error() }

func (pending *MemoryReadPending) Unwrap() error { return ErrMemoryReadNotReady }

func IsOnlyMemoryReadPending(err error) bool {
	if err == ErrMemoryReadNotReady {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsOnlyMemoryReadPending(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsOnlyMemoryReadPending(wrapped.Unwrap())
	default:
		return false
	}
}
