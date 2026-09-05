// The objects this artifact serves — address level 2 of `mongo.<object>.<action>`
// (ADR-020 amendment 2026-09-02, NIM-765/NIM-769).
//
// One artifact, eight objects, one body of MongoDB code. Every object is the same
// [object] value with a different action table; the tables ARE the boundary, so
// `instance` cannot reach a user action by accident — that state is simply unknown
// to it. The driver was not split: an action delegates to the very same method on
// [MongoModule] it was dispatched to before, which is why the MongoDB behaviour and
// its tests are untouched by the re-layout.
//
// `command`, `instance` and `user` are the PILOT three. `replicaset`, `role`,
// `collection`, `index` and `database` (NIM-805) are what makes a mongod managed
// rather than merely reachable; they were added beside the first three, which are
// untouched apart from the `decl` wiring below.
//
// The one thing that DID move is the dispatch key. `user` used to be a single state
// carrying `params.state: present|absent`; those two are now two actions at level 3.
// Keeping the verb in a param would have re-admitted at the parameter level exactly
// what NIM-765 removed from the address, and it had the same measurable cost the
// redis `cluster` action had (NIM-766): both halves shared ONE declared input, so
// `absent` was promised `roles` and `user_password` and param strictness had no
// contract to hold either to.
package main

import (
	"context"
	"fmt"
	"sort"
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
//
// Exactly one of apply / applyOwn is set. apply gets a connection already open to
// `params.addr`; applyOwn gets none, because it owns its connection lifecycle (both
// user actions, which decide the auth path from the live state — see the
// localhost-exception in user.go).
type action struct {
	validate func(f map[string]*structpb.Value) []string
	apply    func(m *MongoModule, ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error

	applyOwn func(m *MongoModule, ctx context.Context, stream eventStream, params *structpb.Struct) error
}

// object is one addressable object of this artifact — the `instance` in
// `mongo.instance.pinged`. It serves the actions in its table and nothing else.
//
// It implements SoulModule, so the value goes straight into [module.Def].Impl;
// BaseModule supplies the no-op Plan, which keeps the deliberate default-deny on
// dry_run (no PlanReadSafe) and on Errand (no ErrandReadSafe) this plugin has had
// since the PILOT slice.
type object struct {
	module.BaseModule

	// impl is the shared MongoDB implementation. Eight objects, one driver.
	impl *MongoModule

	// name is address level 2 — used in diagnostics only; what an operator
	// actually addresses is the registration alias plus this name.
	name string

	// decl is what this object's Def declares about each of its actions — the
	// same map, from the same function, not a copy. Validate and Apply refuse a
	// param whose value is not of the declared type (params.go, NIM-800), so the
	// declaration is load-bearing at runtime and not only in the schema document.
	decl map[string]module.State

	actions map[string]action
}

// Validate performs runtime checks on top of the static ones from soul-lint.
// Returns a ValidateReply with errors (not an error) — that is the Validate
// contract. Error text does NOT contain the password.
func (o *object) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	act, ok := o.actions[req.GetState()]
	if !ok {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{o.unknownState(req.GetState())}}, nil
	}
	// Types before content: an action's own checks read the values, and a value
	// of the wrong type makes whatever they report about it noise.
	if errs := checkParamTypes(o.decl[req.GetState()].Input, req.GetParams().GetFields()); len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	if errs := act.validate(req.GetParams().GetFields()); len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Apply dispatches by state within this object. The final event carries
// changed/failed + output (ADR-012). Connection errors are sanitized (redactError)
// — the address is preserved for diagnostics, the password stripped.
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
	// coerced (params.go, NIM-800). Here rather than only in Validate because a
	// runner need not call Validate at all — the runtime calls Apply — and the
	// value this protects decides whether the password goes out over TLS.
	if errs := checkParamTypes(o.decl[req.GetState()].Input, req.GetParams().GetFields()); len(errs) > 0 {
		return sendFailure(stream, strings.Join(errs, "; "))
	}

	// Then the action's own static checks — the SAME function Validate runs, on the
	// path a runner is guaranteed to take.
	//
	// NIM-786 is usually read in one direction, "Validate must refuse what Apply
	// will", and the reverse gap is the quieter one: `member-removed` with an empty
	// `host` matches no live member and reports "not in the set (no-op)" — success,
	// changed=false, for a step that did nothing anyone asked for. Every validate
	// here is a pure function of the params and opens no socket, so running it twice
	// costs nothing and closes the class rather than three instances of it.
	if errs := act.validate(req.GetParams().GetFields()); len(errs) > 0 {
		return sendFailure(stream, strings.Join(errs, "; "))
	}

	// An action that decides its own auth path (the user object: with auth, then
	// the no-auth localhost-exception fallback for the first admin; the replicaset
	// object, which must also hop to the primary) has no connection to be handed
	// and opens one itself.
	if act.applyOwn != nil {
		return act.applyOwn(o.impl, ctx, stream, req.GetParams())
	}

	cfg, err := parseConnConfig(req.GetParams())
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	conn, err := o.impl.openConn(ctx, cfg)
	if err != nil {
		// Redact BOTH password and PEM client-key: a TLS handshake error could
		// theoretically carry the client-key (security invariant ADR-010, same as
		// the password).
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	return act.apply(o.impl, ctx, stream, conn, req.GetParams())
}

// unknownState names the object as well as the state: with three objects in one
// artifact, "unknown state" alone would leave an author guessing whether the word
// is wrong or the object is.
func (o *object) unknownState(state string) string {
	return fmt.Sprintf("unknown state %q for object %q (expected %s)",
		state, o.name, strings.Join(o.states(), "|"))
}

// states returns the action names this object serves, for the guard that keeps the
// schema document and the dispatch table from drifting apart.
func (o *object) states() []string {
	names := make([]string, 0, len(o.actions))
	for name := range o.actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
