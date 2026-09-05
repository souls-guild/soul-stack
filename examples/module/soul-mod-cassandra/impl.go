// soul-mod-cassandra is a SoulModule plugin for Soul Stack: the interface to a live
// Cassandra cluster. The service's scenario orchestrates order and targeting; the
// plugin performs ONE operation against ONE cluster, speaking CQL through the native
// driver — NOT `core.exec` + `cqlsh` (a password in argv is visible to `ps` on the
// host, and parsing cqlsh output is fragile; same reasoning as the redis and mongo
// plugins).
//
// Five objects, address `cassandra.<object>.<action>` (object.go, ADR-020 amendment
// 2026-09-02):
//   - cassandra.instance.pinged — liveness and version from system.local (read-only,
//     changed=false by design);
//   - cassandra.keyspace.present / .absent — CREATE / ALTER / DROP KEYSPACE;
//   - cassandra.role.present / .absent — CREATE / ALTER / DROP ROLE (Cassandra has
//     roles, not users);
//   - cassandra.table.present / .absent — CREATE / DROP TABLE;
//   - cassandra.command.run — raw CQL (imperative verb-action, changed from params).
//
// Deliberately without a dry-run preview: the plugin sits on BaseModule and does NOT
// implement PlanReadSafe, so the host applies default-deny on dry_run (an honest
// "drift unsupported"), and ErrandReadSafe is likewise absent (default-deny on
// Errand). Same choice as the other two artifacts.
//
// ★ SCHEMA CONVERGENCE. Cassandra propagates a schema change through gossip, so the
// nodes do not agree the instant a DDL statement returns. A state that reported
// `changed` before agreement would leave the cluster where the NEXT step fails on a
// keyspace that "does not exist". Every DDL-issuing action here therefore waits for
// agreement before it reports, and a wait that runs out is a FAILURE, not a quiet
// success (schema.go). The wait is inside the state on purpose: an external probe
// plus a `when:` over it would be a second guard of the same invariant, and two
// guards drift apart.
//
// CRITICAL SECURITY (ADR-010): params["password"] and params["role_password"] NEVER
// reach ApplyEvent.Message, .Output, error text or stderr; all errors are sanitized
// through redactError.
//
// The two do NOT travel the same way, and the difference is worth stating here
// rather than only at the one call site. `password` goes into the connection
// credentials and appears in no statement, ever. `role_password` is written into the
// CREATE ROLE statement as a quoted literal (cql.go, role.go) — because the driver
// binds values only on DML, so a bind marker there would reach the server with
// nothing bound to it. That means a cluster with audit or slow-query logging can
// record it, which is inherent to Cassandra role management and true of cqlsh too.
// What this artifact guarantees is the client side: the statement text is never put
// into an event, a message or an error.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/protobuf/types/known/structpb"
)

// defaultPort is the CQL native transport port, applied to any contact point given
// without one.
const defaultPort = 9042

// CassandraModule is the shared implementation behind every object of this artifact.
// Five objects, one driver: an [object]'s action table delegates to the applyXxx
// methods here, and the object is what a task addresses.
//
// BaseModule provides the no-op Plan (without PlanReadSafe → default-deny on
// dry_run) and deliberately does not implement ErrandReadSafe. The SoulModule
// surface — Validate and Apply — is implemented by [object].
type CassandraModule struct {
	module.BaseModule

	// connect is the injection point for tests. nil → the real driver session.
	connect func(ctx context.Context, cfg connConfig) (cassSession, error)
}

// cassSession is the narrow interface over the driver session, so every action is
// exercised without a live cluster.
type cassSession interface {
	// Query runs a statement that returns rows, with values bound as parameters.
	Query(ctx context.Context, stmt string, args ...any) ([]map[string]any, error)
	// Exec runs a statement that returns no rows.
	Exec(ctx context.Context, stmt string, args ...any) error
	// AwaitSchemaAgreement blocks until every reachable node reports the same
	// schema version, or the driver's own budget runs out. See schema.go for why
	// this is the driver's implementation and not a hand-rolled poll.
	AwaitSchemaAgreement(ctx context.Context) error
	Close()
}

// connConfig is the connection parameters. password and the tls PEM fields are kept
// apart from everything that reaches an event (security invariant ADR-010).
type connConfig struct {
	hosts    []string
	port     int
	keyspace string // session keyspace; set only by an object declaring sessionKeyspace
	username string
	password string
	tls      tlsParams
}

// parseConnConfig extracts the connection parameters. sessionKeyspace decides
// whether `params.keyspace` names the keyspace to CONNECT INTO or merely the one the
// subject lives in — see [object.sessionKeyspace] for why that is not a free choice.
func parseConnConfig(sessionKeyspace bool, s *structpb.Struct) (connConfig, error) {
	f := s.GetFields()
	hosts, err := parseHosts(f["hosts"])
	if err != nil {
		return connConfig{}, err
	}
	port := intOrDefault(f["port"], defaultPort)
	if port < 1 || port > 65535 {
		return connConfig{}, fmt.Errorf("params.port: must be a TCP port in 1..65535, got %d", port)
	}
	cfg := connConfig{
		hosts:    hosts,
		port:     port,
		username: stringOrEmpty(f["username"]),
		password: stringOrEmpty(f["password"]),
		tls:      parseTLS(f),
	}
	if sessionKeyspace {
		cfg.keyspace = stringOrEmpty(f["keyspace"])
		// Checked HERE and not only in the action's validate: the driver issues a
		// USE with this value, and Cassandra folds an unquoted identifier to
		// lowercase — so `App` would silently connect the session to `app`, a
		// different keyspace than the author wrote. Apply is the path a runner
		// actually takes.
		if cfg.keyspace != "" {
			if err := checkIdentifier("params.keyspace", cfg.keyspace); err != nil {
				return connConfig{}, err
			}
		}
	}
	return cfg, nil
}

// parseHosts reads the contact points. A CQL client is given SEVERAL of them and
// discovers the rest of the ring itself, which is why this is a list and not the
// single `addr` the redis and mongo artifacts take: one address would be a lie about
// how the driver reaches the cluster.
func parseHosts(v *structpb.Value) ([]string, error) {
	hosts, err := stringList(v, "params.hosts")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hosts))
	for i, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, fmt.Errorf("params.hosts[%d]: must be a non-empty host or host:port", i)
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("params.hosts: must be a non-empty list of contact points (host or host:port)")
	}
	return out, nil
}

func (m *CassandraModule) openSession(ctx context.Context, cfg connConfig) (cassSession, error) {
	if m.connect != nil {
		return m.connect(ctx, cfg)
	}
	return defaultConnect(ctx, cfg)
}
