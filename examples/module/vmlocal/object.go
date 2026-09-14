// The objects this artifact serves — address level 2 of `<alias>.<object>.<action>`.
//
// One artifact, one object, one body of libvirt code. The action table IS the
// boundary: a state that is not in it is unknown to the object rather than
// reaching some other code path by accident.
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

// eventStream is the Apply server stream, named once so the action table stays
// readable.
type eventStream = grpc.ServerStreamingServer[pluginv1.ApplyEvent]

// action is one state of an object — address level 3.
type action struct {
	// validate runs the structural checks that need no hypervisor. It returns the
	// messages for ValidateReply.Errors, not an error: a bad param is an answer,
	// not a transport failure.
	validate func(params map[string]any) []string
	apply    func(m *VMLocal, ctx context.Context, stream eventStream, params *structpb.Struct) error
}

// object is one addressable object of this artifact. It serves the actions in its
// table and nothing else.
//
// It implements SoulModule, so the value goes straight into [module.Def].Impl;
// BaseModule supplies the no-op Plan, which keeps the default-deny on dry_run (no
// PlanReadSafe) and on Errand (no ErrandReadSafe). Both are deliberate: every
// action here creates, deletes or resizes a machine, and `probed` is a read that
// answers about the hypervisor rather than about drift.
type object struct {
	module.BaseModule

	impl *VMLocal

	// name is address level 2 — used in diagnostics only; what an operator
	// actually addresses is the registration alias plus this name.
	name string

	actions map[string]action
}

// vm binds the object's actions to the shared implementation. The table is the
// object's boundary: nothing else in this artifact is reachable through it.
func (m *VMLocal) vm() *object {
	return &object{
		impl: m,
		name: "vm",
		actions: map[string]action{
			"created":   {validate: validateCreated, apply: (*VMLocal).applyCreated},
			"destroyed": {validate: validateVMIDs, apply: (*VMLocal).applyDestroyed},
			"probed":    {validate: validateProbed, apply: (*VMLocal).applyProbed},
			"resized":   {validate: validateResized, apply: (*VMLocal).applyResized},
		},
	}
}

// Validate performs the checks that need no hypervisor, on top of the static ones
// soul-lint makes from the schema document. Returns a ValidateReply with errors
// (not an error) — that is the Validate contract.
func (o *object) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	act, ok := o.actions[req.GetState()]
	if !ok {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{o.unknownState(req.GetState())}}, nil
	}
	errs := append(undeclaredParams(req.GetState(), req.GetParams().AsMap()), act.validate(req.GetParams().AsMap())...)
	if len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// undeclaredParams refuses a step param this state does not declare.
//
// ★★ It looks redundant with param-level strictness (ADR-0076), and on the path
// where that runs it is. It does not always run: soul-lint walks the tasks in the
// AST of the document it was handed and does not reach a step behind an
// `include:` (NIM-779/NIM-785), so a service that keeps its plugin step in a
// service-level include — which is the layout the published redis service uses —
// has its params checked by NOBODY. There, an undeclared key is dropped in
// silence.
//
// That is the same hole the profile vocabulary closes one level down, for the
// same reason: a key the operator wrote and this artifact ignores is a property
// they asked for and did not get. The artifact is the last place that can say so,
// so it says so.
//
// The declaration is read from [vmDef], not restated, so this cannot drift from
// what the document publishes.
func undeclaredParams(state string, params map[string]any) []string {
	st, ok := vmDef(&VMLocal{}).States[state]
	if !ok {
		return nil
	}
	var unknown []string
	for key := range params {
		if _, declared := st.Input[key]; !declared {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	declared := make([]string, 0, len(st.Input))
	for key := range st.Input {
		declared = append(declared, key)
	}
	sort.Strings(declared)
	out := make([]string, 0, len(unknown))
	for _, key := range unknown {
		out = append(out, fmt.Sprintf("%s is not a param of vm.%s. The params are: %s.",
			key, state, strings.Join(declared, ", ")))
	}
	return out
}

// Apply dispatches by state within this object. The final event carries
// changed/failed plus the output that becomes `register.<task>.*` (ADR-012).
//
// The action's own checks run FIRST, and that is not redundant with the Validate
// RPC: Validate is a separate call a runner is not obliged to make, and every
// action here mutates a machine. Whatever Validate refuses, Apply refuses on the
// same code path (NIM-786) — they are literally the same function.
func (o *object) Apply(req *pluginv1.ApplyRequest, stream eventStream) error {
	act, ok := o.actions[req.GetState()]
	if !ok {
		return sendFailure(stream, o.unknownState(req.GetState()))
	}
	errs := append(undeclaredParams(req.GetState(), req.GetParams().AsMap()), act.validate(req.GetParams().AsMap())...)
	if len(errs) > 0 {
		return sendFailure(stream, "invalid params: "+strings.Join(errs, "; "))
	}
	return act.apply(o.impl, stream.Context(), stream, req.GetParams())
}

// unknownState names the object as well as the state: an "unknown state" alone
// would leave an author guessing whether the word is wrong or the object is.
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

// sendFailure is the final event for a refusal that has no class to report — a
// state this object does not serve, or a param the action cannot work with.
func sendFailure(stream eventStream, msg string) error {
	return stream.Send(&pluginv1.ApplyEvent{Message: msg, Failed: true})
}
