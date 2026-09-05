// The objects this artifact serves — address level 2 of `cassandra.<object>.<action>`
// (ADR-020 amendment 2026-09-02, NIM-765).
//
// One artifact, five objects, one body of Cassandra code. Every object is the same
// [object] value with a different action table; the tables ARE the boundary, so
// `instance` cannot reach a keyspace action by accident — that state is simply
// unknown to it.
//
// The verb lives at address level 3, never in a param. `keyspace.present` and
// `keyspace.absent` are two actions rather than one state carrying
// `params.state: present|absent`, which is what lets each declare only what it
// reads: `absent` is not promised `replication`, and param strictness has a contract
// to hold both to.
package main

import (
	"context"
	"fmt"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// eventStream is the Apply server stream, named once so the action tables stay
// readable.
type eventStream = grpc.ServerStreamingServer[pluginv1.ApplyEvent]

// action is one state of an object — address level 3.
type action struct {
	validate func(f map[string]*structpb.Value) []string
	apply    func(m *CassandraModule, ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error
}

// object is one addressable object of this artifact — the `keyspace` in
// `cassandra.keyspace.present`. It serves the actions in its table and nothing else.
//
// It implements SoulModule, so the value goes straight into [module.Def].Impl;
// BaseModule supplies the no-op Plan, which keeps the deliberate default-deny on
// dry_run (no PlanReadSafe) and on Errand (no ErrandReadSafe).
type object struct {
	module.BaseModule

	// impl is the shared Cassandra implementation. Five objects, one driver.
	impl *CassandraModule

	// name is address level 2 — used in diagnostics only; what an operator actually
	// addresses is the registration alias plus this name.
	name string

	// decl is what this object's Def declares about each of its actions — the same
	// map, from the same function, not a copy. Validate and Apply refuse a param
	// whose value is not of the declared type (params.go, NIM-778), so the
	// declaration is load-bearing at runtime and not only in the schema document.
	decl map[string]module.State

	// sessionKeyspace says whether `params.keyspace` names the keyspace the session
	// CONNECTS INTO. Only `command` says true, and not as a matter of taste: the
	// driver issues a USE on connect, so a session keyspace that does not exist yet
	// fails the connection itself. `table` therefore leaves it false and fully
	// qualifies `<keyspace>.<table>` in every statement — which is what lets
	// `table.present` report "keyspace X does not exist" instead of dying in the
	// connect path with an error about something the author did not write.
	sessionKeyspace bool

	actions map[string]action
}

// Validate performs runtime checks on top of the static ones from soul-lint. Returns
// a ValidateReply with errors (not an error) — that is the Validate contract. Error
// text does NOT contain a password.
func (o *object) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	act, ok := o.actions[req.GetState()]
	if !ok {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{o.unknownState(req.GetState())}}, nil
	}
	// Types before content: an action's own checks read the values, and a value of
	// the wrong type makes whatever they report about it noise.
	if errs := checkParamTypes(o.decl[req.GetState()].Input, req.GetParams().GetFields()); len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	if errs := act.validate(req.GetParams().GetFields()); len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Apply dispatches by state within this object. The final event carries
// changed/failed + output (ADR-012). Connection errors are sanitized (redactError) —
// the contact points are preserved for diagnostics, the password stripped.
//
// The unknown-state refusal is here as well as in Validate, and not for symmetry: a
// runner need not call Validate at all — the runtime calls Apply — so an object
// asked for another object's action has to say no on this path too.
func (o *object) Apply(req *pluginv1.ApplyRequest, stream eventStream) error {
	ctx := stream.Context()

	act, ok := o.actions[req.GetState()]
	if !ok {
		return sendFailure(stream, o.unknownState(req.GetState()))
	}

	// Before anything opens a socket: a param of the wrong type is refused, not
	// coerced (params.go, NIM-778). Here rather than only in Validate because a
	// runner need not call Validate at all, and the value this protects decides
	// whether the password goes out over TLS.
	if errs := checkParamTypes(o.decl[req.GetState()].Input, req.GetParams().GetFields()); len(errs) > 0 {
		return sendFailure(stream, strings.Join(errs, "; "))
	}

	cfg, err := parseConnConfig(o.sessionKeyspace, req.GetParams())
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	session, err := o.impl.openSession(ctx, cfg)
	if err != nil {
		// Redact BOTH the password and the PEM client-key: a TLS handshake error
		// could theoretically carry the client-key (security invariant ADR-010, same
		// as the password).
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer session.Close()

	return act.apply(o.impl, ctx, stream, session, req.GetParams())
}

// unknownState names the object as well as the state: with five objects in one
// artifact, "unknown state" alone would leave an author guessing whether the word is
// wrong or the object is.
func (o *object) unknownState(state string) string {
	return fmt.Sprintf("unknown state %q for object %q (expected %s)",
		state, o.name, strings.Join(o.states(), "|"))
}

// states returns the action names this object serves, for the guard that keeps the
// schema document and the dispatch table from drifting apart.
func (o *object) states() []string {
	return sortedKeys(o.actions)
}
