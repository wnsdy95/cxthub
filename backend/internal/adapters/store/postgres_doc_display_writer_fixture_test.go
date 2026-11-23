//go:build postgres

package store

import (
	"sync"
)

var docDisplayOwnedFixturesPG sync.Map
