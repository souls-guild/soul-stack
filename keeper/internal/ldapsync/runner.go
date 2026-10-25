package ldapsync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	keeperldap "github.com/souls-guild/soul-stack/keeper/internal/auth/ldap"
	"github.com/souls-guild/soul-stack/keeper/internal/leaderloop"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// LeaderLeaseKey is the Redis lease of the sync leader.
const LeaderLeaseKey = "ldapsync:leader"

// tickInterval is how often the leader asks whether a pass is due, not how
// often anyone is looked up: that is the interval in the settings, measured
// from the latest attempt in the database. A short tick makes an edited
// interval apply within it and makes the immediate tick a new leader runs on
// acquire harmless.
const tickInterval = 30 * time.Second

const lockTTL = 90 * time.Second

// RunnerConfig wires a [Runner].
type RunnerConfig struct {
	Holder  string
	Redis   *redis.Client
	Logger  *slog.Logger
	Syncer  *Syncer
	Metrics *Metrics
}

// Runner keeps the sync running on exactly one instance of the cluster.
type Runner struct {
	loop *leaderloop.Loop
}

// NewRunner builds the leader loop around the syncer.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Syncer == nil {
		return nil, errors.New("ldapsync: Syncer is required")
	}
	loop, err := leaderloop.New(leaderloop.Config{
		LeaseKey:      LeaderLeaseKey,
		Holder:        cfg.Holder,
		Redis:         cfg.Redis,
		Logger:        cfg.Logger,
		IntervalFn:    func() time.Duration { return tickInterval },
		LockTTLFn:     func() time.Duration { return lockTTL },
		Tick:          func(ctx context.Context) { cfg.Syncer.Run(ctx) },
		OnLeaseChange: cfg.Metrics.SetLeaseHeld,
	})
	if err != nil {
		return nil, err
	}
	return &Runner{loop: loop}, nil
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error { return r.loop.Run(ctx) }

// LDAPDirectory adapts the login authenticator to [DirectoryOpener]: the sync
// reads the directory with the connection settings and service account a
// login uses.
type LDAPDirectory struct {
	Authenticator *keeperldap.Authenticator
}

// OpenDirectory implements [DirectoryOpener].
func (d LDAPDirectory) OpenDirectory(ctx context.Context) (Directory, error) {
	dir, err := d.Authenticator.OpenDirectory(ctx)
	if err != nil {
		return nil, err
	}
	return dir, nil
}
