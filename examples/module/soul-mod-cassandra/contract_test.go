// The two contracts every object of this artifact is held to, checked over ALL of
// them at once rather than object by object: a rule that has to be remembered per
// object is a rule that will be forgotten on the sixth.
package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// objectSet is every object of the artifact, looked up by address level 2. The
// lookup panics on an unknown name so a renamed object breaks the tables in this
// file instead of silently skipping the rows that named it.
type objectSet map[string]*object

func (o objectSet) get(name string) *object {
	obj, ok := o[name]
	if !ok {
		panic("no such object: " + name)
	}
	return obj
}

// allObjects is every object this artifact serves, by address level 2.
func allObjects(m *CassandraModule) objectSet {
	return objectSet{
		"command":  m.command(),
		"instance": m.instance(),
		"keyspace": m.keyspace(),
		"role":     m.role(),
		"table":    m.table(),
	}
}

// validParams is a call that must be accepted, per object and action. It doubles as
// the base a contract case perturbs one key of.
func validParams(object, state string) map[string]any {
	switch object + "." + state {
	case "instance.pinged":
		return connParams()
	case "keyspace.present":
		return keyspaceParams(nil)
	case "keyspace.absent":
		return withParams(map[string]any{"name": "app"})
	case "role.present":
		return withParams(map[string]any{"name": "app_rw", "role_password": secretRolePass})
	case "role.absent":
		return withParams(map[string]any{"name": "app_rw"})
	case "table.present":
		return tableParams(nil)
	case "table.absent":
		return withParams(map[string]any{"keyspace": "app", "name": "events"})
	case "command.run":
		return withParams(map[string]any{"cql": "SELECT release_version FROM system.local"})
	default:
		return nil
	}
}

// TestEveryActionAcceptsItsValidCall — the floor under every refusal test below. A
// contract that refuses everything would pass them all.
func TestEveryActionAcceptsItsValidCall(t *testing.T) {
	for name, obj := range allObjects(&CassandraModule{}) {
		for _, state := range obj.states() {
			t.Run(name+"."+state, func(t *testing.T) {
				params := validParams(name, state)
				if params == nil {
					t.Fatalf("no valid call is written for %s.%s — a new action needs a row in validParams", name, state)
				}
				reply, err := obj.Validate(context.Background(), &pluginv1.ValidateRequest{
					State: state, Params: mustStruct(t, params),
				})
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if !reply.GetOk() {
					t.Fatalf("Validate refused a valid call: %v", reply.GetErrors())
				}
			})
		}
	}
}

// ★ NIM-778 — a param of the wrong type is REFUSED, never coerced.
//
// The live defect this replaces: `tls: "true"` written as a string fell back to
// `false` on the redis artifact, and the connection — password included — went out in
// plaintext, reported as reconciled. The direction of the fallback is what made it a
// leak: `false` is the insecure side. Nothing upstream catches it, because the
// Keeper's static check returns nil on a `${…}` cell and the runtime calls Apply
// rather than Validate.
func TestWrongTypedParamIsRefusedByEveryAction(t *testing.T) {
	// Each perturbation is a value of the wrong type for a param every action
	// declares, so one table covers all nine.
	perturbations := []struct {
		key   string
		value any
		want  string
	}{
		{"tls", "true", "must be a boolean"},
		{"tls_skip_verify", "yes", "must be a boolean"},
		{"port", "9042", "must be an integer"},
		{"port", 9042.5, "must be an integer"},
		{"hosts", "10.0.0.1", "must be a list"},
		{"password", 42, "must be a string"},
	}

	for name, obj := range allObjects(&CassandraModule{}) {
		for _, state := range obj.states() {
			for _, p := range perturbations {
				t.Run(name+"."+state+"/"+p.key, func(t *testing.T) {
					params := validParams(name, state)
					params[p.key] = p.value

					reply, err := obj.Validate(context.Background(), &pluginv1.ValidateRequest{
						State: state, Params: mustStruct(t, params),
					})
					if err != nil {
						t.Fatalf("Validate: %v", err)
					}
					if reply.GetOk() {
						t.Errorf("Validate accepted %s = %#v", p.key, p.value)
					} else if !strings.Contains(strings.Join(reply.GetErrors(), "; "), p.want) {
						t.Errorf("Validate refusal does not say %q: %v", p.want, reply.GetErrors())
					}

					// And Apply refuses it too, WITHOUT opening a connection: a
					// runner need not call Validate at all, and the value this
					// protects decides whether the password goes out over TLS.
					connected := false
					m := &CassandraModule{connect: func(context.Context, connConfig) (cassSession, error) {
						connected = true
						return &fakeSession{}, nil
					}}
					stream := apply(t, allObjects(m)[name], state, params)
					assertOutcome(t, stream, false, true)
					if connected {
						t.Error("Apply opened a connection before refusing a wrong-typed param")
					}
				})
			}
		}
	}
}

// ★ NIM-786 — Validate refuses what Apply refuses.
//
// The live defect this replaces: a node-spec error was swallowed on the redis
// artifact, so Validate was clean and Apply died in the middle of a run. The point of
// the phase is to say no BEFORE anything happened; whatever it lets through silently
// makes the phase worthless.
func TestValidateRefusesWhateverApplyRefuses(t *testing.T) {
	cases := []struct {
		object, state string
		params        map[string]any
	}{
		{"instance", "pinged", withParams(map[string]any{"hosts": []any{}})},
		{"instance", "pinged", withParams(map[string]any{"hosts": []any{""}})},
		{"instance", "pinged", withParams(map[string]any{"port": 0})},
		{"instance", "pinged", withParams(map[string]any{"port": 70000})},
		{"keyspace", "present", keyspaceParams(map[string]any{"name": "App"})},
		{"keyspace", "present", keyspaceParams(map[string]any{"name": ""})},
		{"keyspace", "present", keyspaceParams(map[string]any{"replication": map[string]any{"class": networkStrategy, "dc1": "3"}})},
		{"keyspace", "present", keyspaceParams(map[string]any{"replication": map[string]any{"class": "Nonsense", "dc1": 3}})},
		{"keyspace", "absent", withParams(map[string]any{"name": "app-1"})},
		{"role", "present", withParams(map[string]any{"name": "App_RW"})},
		{"role", "absent", withParams(map[string]any{"name": ""})},
		{"table", "present", tableParams(map[string]any{"partition_key": []any{"nope"}})},
		{"table", "present", tableParams(map[string]any{"columns": map[string]any{"id": "uuid; DROP TABLE x"}, "partition_key": []any{"id"}, "clustering_key": []any{}})},
		{"table", "present", tableParams(map[string]any{"keyspace": "App"})},
		{"table", "absent", withParams(map[string]any{"keyspace": "app", "name": "Events"})},
		{"command", "run", withParams(map[string]any{"cql": ""})},
		{"command", "run", withParams(map[string]any{"cql": "SELECT 1", "keyspace": "App"})},
	}

	for _, tc := range cases {
		t.Run(tc.object+"."+tc.state+"/"+summarize(tc.params), func(t *testing.T) {
			// Apply first, so a case that Apply happens to ACCEPT is reported as a
			// bad case rather than as a Validate hole.
			s := &fakeSession{}
			stream := apply(t, allObjects(newModule(s)).get(tc.object), tc.state, tc.params)
			if !stream.final().GetFailed() {
				t.Fatalf("Apply accepted this call, so it is not a parity case: %v", tc.params)
			}

			reply, err := allObjects(&CassandraModule{})[tc.object].Validate(context.Background(),
				&pluginv1.ValidateRequest{State: tc.state, Params: mustStruct(t, tc.params)})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if reply.GetOk() {
				t.Errorf("Apply refuses this and Validate does not — the refusal moved into the middle of the run (NIM-786)")
			}
		})
	}
}

// TestUnknownStateIsRefusedOnBothPaths — an object asked for another object's action
// has to say no on the Apply path too: a runner need not call Validate at all.
func TestUnknownStateIsRefusedOnBothPaths(t *testing.T) {
	for name, obj := range allObjects(newModule(&fakeSession{})) {
		t.Run(name, func(t *testing.T) {
			reply, _ := obj.Validate(context.Background(), &pluginv1.ValidateRequest{
				State: "not-a-state", Params: mustStruct(t, connParams()),
			})
			if reply.GetOk() {
				t.Error("Validate accepted an unknown state")
			}

			stream := apply(t, obj, "not-a-state", connParams())
			e := assertOutcome(t, stream, false, true)
			// The message names the OBJECT as well as the state: with five objects in
			// one artifact, "unknown state" alone leaves an author guessing which of
			// the two words is wrong.
			if !strings.Contains(e.GetMessage(), name) {
				t.Errorf("the refusal must name the object %q, got %q", name, e.GetMessage())
			}
		})
	}
}

// TestSecretsReachTheConnectionAndNothingElse — the password belongs in the
// credentials and nowhere else (ADR-010).
func TestSecretsReachTheConnectionAndNothingElse(t *testing.T) {
	for name, obj := range allObjects(&CassandraModule{}) {
		for _, state := range obj.states() {
			t.Run(name+"."+state, func(t *testing.T) {
				s := &fakeSession{}
				stream := apply(t, allObjects(newModule(s))[name], state, validParams(name, state))
				if s.cfg.password != secretPass {
					t.Errorf("the password did not reach the connection: %q", s.cfg.password)
				}
				assertNoSecretInEvents(t, stream)
				assertNoSecretInStatementText(t, s)
			})
		}
	}
}

// summarize names a params map for a subtest, deterministically and without secrets.
func summarize(params map[string]any) string {
	var keys []string
	for _, k := range sortedKeys(params) {
		switch k {
		case "hosts", "username", "password":
			continue
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, "-")
}
