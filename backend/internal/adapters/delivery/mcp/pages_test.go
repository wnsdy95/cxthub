package mcp

import (
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func pageHash(i int) domain.ContentHash { return domain.ContentHash(fmt.Sprintf("sha256:%064x", i)) }
