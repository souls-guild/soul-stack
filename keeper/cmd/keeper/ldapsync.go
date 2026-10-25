package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/ldapsync"
)

// ldapSyncRuns reports whether this instance takes part in the background LDAP
// group sync: LDAP login is configured and Redis is there for the leader lease.
func (d *daemon) ldapSyncRuns() bool {
	return d.ldapAuthn != nil && d.ldapMapper != nil && d.redisClient != nil && d.pool != nil
}

// ldapSyncStoreOrNil keeps a nil pool from becoming a typed-nil interface.
func ldapSyncStoreOrNil(pool *pgxpool.Pool) handlers.LDAPSyncStore {
	if pool == nil {
		return nil
	}
	return ldapsync.PGStore{DB: pool}
}

// setupLDAPSync starts the background LDAP group sync (ADR-058 amendment
// NIM-1053) on every instance; the Redis lease leaves exactly one of them
// working.
func (d *daemon) setupLDAPSync(ctx context.Context) error {
	logger := d.logger
	if d.ldapAuthn == nil {
		return nil
	}
	if !d.ldapSyncRuns() {
		logger.Warn("keeper run: LDAP group sync disabled — no Redis for the leader lease; roles from LDAP groups are reconciled only at login")
		return nil
	}

	metrics := ldapsync.RegisterMetrics(d.metricsReg)
	syncer, err := ldapsync.NewSyncer(ldapsync.Config{
		Store:       ldapsync.PGStore{DB: d.pool},
		Directory:   ldapsync.LDAPDirectory{Authenticator: d.ldapAuthn},
		Roles:       d.ldapMapper,
		Audit:       d.auditWriter,
		Invalidator: newRBACInvalidator(d.rbacHolder, d.redisClient, d.cfg.KID, logger),
		Metrics:     metrics,
		Logger:      logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build LDAP group sync: %v\n", err)
		return errSetupFailed
	}
	runner, err := ldapsync.NewRunner(ldapsync.RunnerConfig{
		Holder:  d.cfg.KID,
		Redis:   d.redisClient,
		Logger:  logger,
		Syncer:  syncer,
		Metrics: metrics,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "keeper run: build LDAP group sync: %v\n", err)
		return errSetupFailed
	}

	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.cleanups.push(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Warn("LDAP group sync did not stop within 5s after shutdown — leak suspected")
		}
	})
	d.cleanups.push(cancel)
	go func() {
		defer close(done)
		if err := runner.Run(syncCtx); err != nil {
			logger.Error("LDAP group sync stopped with error", slog.Any("error", err))
		}
	}()
	return nil
}
