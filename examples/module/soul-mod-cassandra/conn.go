// The real driver session, split from impl.go so every action is tested against a
// fake [cassSession] without a live cluster.
//
// Credentials are set through the driver's typed fields (PasswordAuthenticator,
// SslOptions), never by interpolating them into a connection string: a password in a
// URL would need escaping and could surface in driver diagnostics.
package main

import (
	"context"

	"github.com/apache/cassandra-gocql-driver/v2"
)

// defaultConnect opens a real session. The driver's own connect already proves TCP,
// the CQL handshake and (with credentials) authentication, so there is no separate
// ping here.
func defaultConnect(_ context.Context, cfg connConfig) (cassSession, error) {
	cluster := gocql.NewCluster(cfg.hosts...)
	// A contact point may carry its own port; this is the default for the ones that
	// do not, and for the peers the driver discovers.
	cluster.Port = cfg.port
	cluster.Keyspace = cfg.keyspace

	if cfg.username != "" || cfg.password != "" {
		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: cfg.username,
			Password: cfg.password,
		}
	}

	tlsCfg, err := buildTLSConfig(cfg.tls)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		// ★ SslOptions is insecure by DEFAULT in this driver, and the two flags
		// interact: EnableHostVerification can only tighten a Config that is already
		// verifying, so leaving it false while passing a verifying Config is not
		// enough to be sure. Both are set from the same operator decision, which
		// makes the pair unambiguous in either direction — the ADR-010 default-secure
		// stance cannot be lost to a driver default.
		cluster.SslOpts = &gocql.SslOptions{
			Config:                 tlsCfg,
			EnableHostVerification: !cfg.tls.skipVerify,
		}
	}

	session, err := cluster.CreateSession()
	if err != nil {
		return nil, err
	}
	return &realSession{s: session}, nil
}

// realSession adapts *gocql.Session to [cassSession].
type realSession struct {
	s *gocql.Session
}

func (r *realSession) Query(ctx context.Context, stmt string, args ...any) ([]map[string]any, error) {
	iter := r.s.Query(stmt, args...).WithContext(ctx).Iter()
	rows, err := iter.SliceMap()
	if closeErr := iter.Close(); err == nil {
		err = closeErr
	}
	return rows, err
}

func (r *realSession) Exec(ctx context.Context, stmt string, args ...any) error {
	return r.s.Query(stmt, args...).WithContext(ctx).Exec()
}

func (r *realSession) AwaitSchemaAgreement(ctx context.Context) error {
	return r.s.AwaitSchemaAgreement(ctx)
}

func (r *realSession) Close() {
	r.s.Close()
}
