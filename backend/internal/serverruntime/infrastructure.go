package serverruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type InfrastructureConfig struct {
	DSN                          string
	DataDir                      string
	RequirePostgres              bool
	MigrationsDirectory          string
	LegacyByteRangePublication   bool
	LegacyDerivedBytePublication bool
	execution                    *executionProfile
}

type Infrastructure struct {
	Store           store.Store
	DocumentProfile inbound.DocumentQueryProfile
	ownedStore      store.Store
	closeOnce       sync.Once
	closeErr        error
	execution       *executionProfile
}

func OpenInfrastructure(ctx context.Context, config InfrastructureConfig) (_ *Infrastructure, err error) {
	dsn := strings.TrimSpace(config.DSN)
	if config.execution != nil {
		dsn, err = executionDataDSN(dsn, config.execution.role)
		if err != nil {
			return nil, err
		}
		config.RequirePostgres = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	storage, err := store.Open(config.DataDir, dsn, config.RequirePostgres)
	if err != nil {
		if config.execution != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: cannot open bounded PostgreSQL store", errors.ErrUnsupported)
		}
		return nil, fmt.Errorf("open store: %w", err)
	}
	infrastructure := &Infrastructure{
		Store:           storage,
		DocumentProfile: documentquery.LegacyCompatibility,
		ownedStore:      storage,
		execution:       config.execution,
	}
	defer func() {
		if err != nil {
			_ = infrastructure.Close()
		}
	}()
	if config.RequirePostgres || dsn != "" {
		if err = app.ValidateProductionStore(storage); err != nil {
			return nil, err
		}
		directory := config.MigrationsDirectory
		if directory == "" {
			directory = "schemas/db/migrations"
		}
		err = storage.VerifyMigrations(ctx, directory)
		if err != nil {
			return nil, fmt.Errorf("verify schema (run cxt-admin migrate --apply before serving): %w", err)
		}
		log.Print("schema: verified")
		infrastructure.DocumentProfile = documentquery.VerifiedRoots
	}
	if config.LegacyDerivedBytePublication && !config.LegacyByteRangePublication {
		return nil, fmt.Errorf("CXT_LEGACY_DERIVED_BYTE_PUBLICATION requires CXT_LEGACY_BYTE_RANGE_PUBLICATION")
	}
	derivedPublication, supportsDerivedPublication := storage.(outbound.LegacyDerivedByteRangePublicationStore)
	if config.LegacyDerivedBytePublication && !supportsDerivedPublication {
		return nil, fmt.Errorf("legacy derived byte-range publication requires a supporting store")
	}
	if _, supportsAccounting := storage.(outbound.LegacyDerivedByteStorageAccounting); config.LegacyDerivedBytePublication && !supportsAccounting {
		return nil, fmt.Errorf("legacy derived byte-range publication requires storage accounting")
	}
	if publication, supported := storage.(outbound.LegacyByteRangePublicationStore); supported {
		publication.ConfigureLegacyByteRangePublication(config.LegacyByteRangePublication)
	} else if config.LegacyByteRangePublication {
		return nil, fmt.Errorf("legacy byte-range publication requires a supporting store")
	}
	if supportsDerivedPublication {
		derivedPublication.ConfigureLegacyDerivedByteRangePublication(config.LegacyDerivedBytePublication)
	}
	if config.execution != nil {
		if err = config.execution.configure(ctx, storage); err != nil {
			return nil, err
		}
	}
	return infrastructure, nil
}

func (infrastructure *Infrastructure) Close() error {
	if infrastructure == nil {
		return nil
	}
	infrastructure.closeOnce.Do(func() {
		infrastructure.closeErr = infrastructure.execution.close()
		switch closer := infrastructure.ownedStore.(type) {
		case interface{ Close() error }:
			storeErr := closer.Close()
			if infrastructure.closeErr == nil {
				infrastructure.closeErr = storeErr
			} else {
				infrastructure.closeErr = errors.Join(infrastructure.closeErr, storeErr)
			}
		case interface{ Close() }:
			closer.Close()
		}
	})
	return infrastructure.closeErr
}
