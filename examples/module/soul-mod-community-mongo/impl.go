// soul-mod-community-mongo is a real SoulModule plugin for Soul Stack
// (community.mongo): main interface to live MongoDB (PILOT slice). The service's
// Scenario orchestrates order/targeting; plugin executes ONE operation on ONE
// mongod instance via go-mongo-driver (NOT core.exec+mongosh: password in argv is
// a security risk with fragile parsing; parallel to community.redis).
//
// States (PILOT):
//   - pinged  — health-probe via driver Ping (read-only, changed=false by design,
//     precedent: core.http.probe / community.redis.pinged);
//   - user    — createUser/dropUser (upsert, imperative — NOT aclfile: in mongo
//     users live in admin.system.users, not in config file). state
//     present/absent. Idempotent by usersInfo. ★ localhost-exception:
//     first admin is created WITHOUT auth via localhost while admin DB
//     is empty (mongo-mechanism, analog to redis default_admin bootstrap);
//   - command — raw db.runCommand (imperative verb-state, changed from params —
//     precedent: community.redis.command).
//
// Intentionally without dry-run preview: plugin on BaseModule does NOT implement
// PlanReadSafe → host applies default-deny (on dry_run — honest "drift unsupported",
// user's choice, parallel to community.redis).
//
// Backend — go.mongodb.org/mongo-driver. Address + password come from Keeper:
// password is already resolved by render phase from vault-ref (ADR-012), plugin does
// NOT fetch its own Vault client (capability — network_outbound only).
//
// CRITICAL SECURITY (ADR-010): params["password"] NEVER goes into ApplyEvent.Message,
// .Output, error text, or stderr. All connection/command errors are sanitized
// (redactError by password).
package main

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// MongoModule is the implementation of SoulModule community.mongo.
//
// BaseModule provides a no-op Plan (without PlanReadSafe → default-deny on dry_run) and
// intentionally does NOT implement ErrandReadSafe (default-deny on Errand). We override
// Validate and Apply.
type MongoModule struct {
	module.BaseModule

	// connect is an injection point for L0. nil → real mongo.Connect.
	connect func(ctx context.Context, cfg connConfig) (mongoConn, error)
}

// mongoConn is a narrow interface over *mongo.Client (for L0 mocking).
type mongoConn interface {
	// Ping checks instance liveness (driver Ping to primary).
	Ping(ctx context.Context) error
	// RunCommand executes a command in database db and returns raw bson response.
	// The response string/fields are server output, not operator secrets.
	RunCommand(ctx context.Context, db string, cmd bson.D) (bson.Raw, error)
	Close(ctx context.Context) error
}

// connConfig is connection parameters. password and tls.*PEM are kept separate and
// NEVER logged or placed in events (security invariant ADR-010).
type connConfig struct {
	addr     string // host:port
	username string
	password string
	authDB   string // authenticationDatabase (usually admin)
	tls      tlsParams
}

// Validate performs runtime checks on top of static checks from soul-lint. Returns
// ValidateReply with errors (not error) — this is the Validate contract. Error text
// does NOT contain the password.
func (m *MongoModule) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	f := req.GetParams().GetFields()

	switch req.GetState() {
	case "pinged":
		errs = append(errs, validateAddr(f)...)
	case "user":
		errs = append(errs, validateUser(f)...)
	case "command":
		errs = append(errs, validateCommand(f)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (expected pinged|user|command)", req.GetState()))
	}

	if len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Apply dispatches by state. The final event carries changed/failed +
// output (ADR-012). Connection/command errors are sanitized (redactError) — address
// is preserved for diagnostics, password is stripped.
//
// ★ user-state opens connection ITSELF (localhost-exception fallback: first admin
// is created without auth) — therefore its connection lifecycle is extracted to applyUser,
// not to the common openConn here. pinged/command go through the common path (connection with auth).
func (m *MongoModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if req.GetState() == "user" {
		return m.applyUser(ctx, stream, req.GetParams())
	}

	cfg, err := parseConnConfig(req.GetParams())
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	conn, err := m.openConn(ctx, cfg)
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	switch req.GetState() {
	case "pinged":
		return m.applyPinged(ctx, stream, conn, cfg.password)
	case "command":
		return m.applyCommand(ctx, stream, conn, req.GetParams(), cfg.password)
	default:
		return sendFailure(stream, fmt.Sprintf("unknown state %q (expected pinged|user|command)", req.GetState()))
	}
}

// parseConnConfig extracts connection parameters from params. password is kept
// separate from everything that goes into events (security invariant ADR-010). authDB
// defaults to admin (mongo's standard authentication database for system users).
func parseConnConfig(s *structpb.Struct) (connConfig, error) {
	f := s.GetFields()
	addr, _ := stringValue(f["addr"])
	if strings.TrimSpace(addr) == "" {
		return connConfig{}, fmt.Errorf("params.addr: must be a non-empty string")
	}
	authDB := stringOrEmpty(f["auth_db"])
	if authDB == "" {
		authDB = "admin"
	}
	return connConfig{
		addr:     addr,
		username: stringOrEmpty(f["username"]),
		password: stringOrEmpty(f["password"]),
		authDB:   authDB,
		tls:      parseTLS(f),
	}, nil
}

func (m *MongoModule) openConn(ctx context.Context, cfg connConfig) (mongoConn, error) {
	if m.connect != nil {
		return m.connect(ctx, cfg)
	}
	return defaultConnect(ctx, cfg)
}

// applyPinged is health-probe via driver Ping. changed=false by design
// (probe, not change): interpretation of "healthy/not" is at the scenario level via
// retry/until/failed_when by register.self.ok.
func (m *MongoModule) applyPinged(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn mongoConn, password string) error {
	if err := conn.Ping(ctx); err != nil {
		return sendFailure(stream, "PING: "+redactError(err, password))
	}
	return sendOutcome(stream, false, "PING ok", map[string]any{
		"ok": true,
	})
}

// applyCommand executes raw db.runCommand (imperative verb-state). changed is
// taken from params.changed (default false, probe-semantics). db is the target
// database (default admin). command is the bson document of the command (first key = command name).
// Output carries the ok flag from the response; password does not go into events.
func (m *MongoModule) applyCommand(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn mongoConn, params *structpb.Struct, password string) error {
	f := params.GetFields()
	db := stringOrEmpty(f["db"])
	if db == "" {
		db = "admin"
	}
	changed := boolOrDefault(f["changed"], false)

	cmd, err := commandDoc(f["command"])
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	raw, err := conn.RunCommand(ctx, db, cmd)
	if err != nil {
		// Command error is server response (not operator secret); connection level could
		// carry password → redactError by password (defense-in-depth).
		return sendFailure(stream, "runCommand: "+redactError(err, password))
	}
	return sendOutcome(stream, changed, "runCommand ok", map[string]any{
		"ok": commandOK(raw),
	})
}

// commandDoc builds bson.D command from params.command (map). First key of mongo command
// must be its name, but map iteration order is nondeterministic — only single-field
// commands are reliable ({ping: 1}, etc.), multi-field order is not guaranteed (pilot limitation).
func commandDoc(v *structpb.Value) (bson.D, error) {
	fields := structField(v)
	if len(fields) == 0 {
		return nil, fmt.Errorf("params.command: must be a non-empty map (bson command document)")
	}
	doc := bson.D{}
	for k, vv := range fields {
		doc = append(doc, bson.E{Key: k, Value: valueToNative(vv)})
	}
	return doc, nil
}

// commandOK extracts field "ok" from a command bson response (1 → true). A successful
// mongo command response carries {ok: 1}. Missing/0 → false.
func commandOK(raw bson.Raw) bool {
	if len(raw) == 0 {
		return false
	}
	v, err := raw.LookupErr("ok")
	if err != nil {
		return false
	}
	switch v.Type {
	case bson.TypeDouble:
		return v.Double() == 1
	case bson.TypeInt32:
		return v.Int32() == 1
	case bson.TypeInt64:
		return v.Int64() == 1
	default:
		return false
	}
}

// validateCommand performs static checks for command-state: addr + command
// are required.
func validateCommand(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	if len(structField(f["command"])) == 0 {
		errs = append(errs, "params.command: must be a non-empty map (bson command document)")
	}
	return errs
}
