package serverruntime

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (runtime *Runtime) ConfigureMemoryDigestCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure memory digest commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.MemoryDigests = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure memory digest commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.MemoryDigestCommands()
	if err != nil {
		return fmt.Errorf("configure memory digest commands: %w", err)
	}
	runtime.MemoryDigests = commands
	return nil
}
