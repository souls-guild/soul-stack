// Guards on the `replicaset` object — the ones that matter are about what it
// REFUSES to do and about what a reconfig it does send is made of.
//
// Every assertion here is on the wire: which command was sent, to which node, and
// what document it carried. None of them assert on prose, because the invariants
// are things like "replSetInitiate is never sent at a set that has a config" and
// "the reconfig keeps the settings block", and neither is visible in a message.
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
)

// rsNode scripts one mongod of a replica set: its config (nil = never initiated)
// and what replSetGetStatus reports. Everything else answers {ok: 1}, including the
// usersInfo probe [MongoModule.openUserConn] makes, so the auth path succeeds and
// no bootstrap fallback fires unless a test asks for one.
func rsNode(t *testing.T, cfg *bson.D, status ...[2]string) *fakeConn {
	t.Helper()
	c := &fakeConn{}
	c.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		switch cmd[0].Key {
		case "replSetGetConfig":
			if cfg == nil {
				return nil, cmdErr(codeNotYetInitialized, "NotYetInitialized")
			}
			return rsConfigReply(t, *cfg), nil
		case "replSetGetStatus":
			return rsStatusReply(t, status...), nil
		case "usersInfo":
			return usersRaw(1), nil
		default:
			return okRaw(), nil
		}
	}
	return c
}

// liveConfig is a three-field config document carrying a `settings` block and a
// `protocolVersion` this artifact does not model — which is the point: they must
// survive every reconfig.
func liveConfig(version int32, members ...bson.D) bson.D {
	arr := bson.A{}
	for _, m := range members {
		arr = append(arr, m)
	}
	return bson.D{
		{Key: "_id", Value: "rs0"},
		{Key: "version", Value: version},
		{Key: "protocolVersion", Value: int64(1)},
		{Key: "members", Value: arr},
		{Key: "settings", Value: bson.D{{Key: "chainingAllowed", Value: false}, {Key: "heartbeatTimeoutSecs", Value: int32(7)}}},
	}
}

func liveMember(id int32, host string, extra ...bson.E) bson.D {
	return append(bson.D{{Key: "_id", Value: id}, {Key: "host", Value: host}}, extra...)
}

// rsApply runs one replicaset action and returns the stream.
func rsApply(t *testing.T, m *MongoModule, state string, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := m.replicaset().Apply(&pluginv1.ApplyRequest{
		State:  state,
		Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	return stream
}

// --- initiated: the three branches ---

// TestRSInitiated_NotInitiated_SendsInitiate is the only path on which
// replSetInitiate may be sent, and the config it carries is built from params
// because there is no live one to derive from.
func TestRSInitiated_NotInitiated_SendsInitiate(t *testing.T) {
	node := rsNode(t, nil, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"c": map[string]any{"host": "mongo-3:27017"},
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017"},
		},
	})

	changed, _ := outcome(t, stream)
	if !changed {
		t.Error("initiating a set that had no config must report changed=true")
	}
	if n := countCommand(node.calls, "replSetInitiate"); n != 1 {
		t.Fatalf("replSetInitiate sent %d times, want exactly 1", n)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("replSetReconfig sent %d times at an uninitiated set — the two are different operations", n)
	}
	if got := outputField(t, stream, "initiated"); got != true {
		t.Errorf("output.initiated = %v, want true", got)
	}
	if got := outputField(t, stream, "primary"); got != "mongo-1:27017" {
		t.Errorf("output.primary = %v, want the elected member", got)
	}
}

// TestRSInitiated_MemberIdsAreDeterministic — the `_id` of a fresh set follows the
// SORTED order of the params keys, not map iteration order, so the same input
// yields the same config every run.
func TestRSInitiated_MemberIdsAreDeterministic(t *testing.T) {
	for i := 0; i < 10; i++ {
		node := rsNode(t, nil, [2]string{"mongo-1:27017", "PRIMARY"})
		m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

		rsApply(t, m, "initiated", map[string]any{
			"addr": "10.0.0.1:27017",
			"name": "rs0",
			"members": map[string]any{
				"charlie": map[string]any{"host": "mongo-3:27017"},
				"alpha":   map[string]any{"host": "mongo-1:27017"},
				"bravo":   map[string]any{"host": "mongo-2:27017"},
			},
		})

		call, ok := lastCommand(node.calls, "replSetInitiate")
		if !ok {
			t.Fatal("no replSetInitiate was sent")
		}
		cfg, ok := call.cmd[0].Value.(bson.D)
		if !ok {
			t.Fatalf("replSetInitiate value is %T, want bson.D", call.cmd[0].Value)
		}
		members := configMembers(t, cfg)
		want := []string{"mongo-1:27017", "mongo-2:27017", "mongo-3:27017"}
		for j, w := range want {
			if h, _ := docField(members[j], "host"); h != w {
				t.Fatalf("member %d is %v, want %s — the layout must follow the sorted keys", j, h, w)
			}
			if id, _ := docField(members[j], "_id"); id != int64(j) {
				t.Fatalf("member %s got _id %v, want %d", w, id, j)
			}
		}
	}
}

// TestRSInitiated_Formed_IsNoOpAndNeverInitiates is the idempotency contract and
// the central safety guard at once: a set that already IS what was declared must
// come back changed=false, and replSetInitiate must not be sent at it — that
// command against a live set is the operation whose cost this object exists to
// avoid paying by accident.
func TestRSInitiated_Formed_IsNoOpAndNeverInitiates(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017"),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"}, [2]string{"mongo-2:27017", "SECONDARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017"},
		},
	})

	changed, msg := outcome(t, stream)
	if changed {
		t.Errorf("a converged set must report changed=false, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetInitiate"); n != 0 {
		t.Errorf("replSetInitiate sent %d times at a LIVE set", n)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("replSetReconfig sent %d times at a converged set — a no-op writes nothing", n)
	}
}

// TestRSInitiated_HostWithoutPortIsNotDrift — mongod stores `mongo-1` as
// `mongo-1:27017`, so a declared host written without a port must compare equal.
// Without the normalization every apply would see a membership that changed.
func TestRSInitiated_HostWithoutPortIsNotDrift(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs0",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1"}},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a host written without a port must match the stored one, got changed=true: %q", msg)
	}
}

// TestRSInitiated_Partial_AddsOnlyMissing is the redis `clusterPartial` shape
// carried across, and it asserts the whole of the reconfig invariant at once:
//
//   - the missing member is added and the present ones are untouched;
//   - existing `_id`s keep their values, and the new one is max+1 rather than an
//     index into the array;
//   - `settings` and `protocolVersion` — fields this artifact does not model —
//     survive, which is what "the document is the LIVE one" buys;
//   - `version` goes up by exactly one;
//   - the reconfig goes to the PRIMARY, dialled through the member's own `addr`,
//     not to `params.addr`.
func TestRSInitiated_Partial_AddsOnlyMissing(t *testing.T) {
	cfg := liveConfig(3,
		liveMember(0, "mongo-1:27017"),
		liveMember(5, "mongo-2:27017", bson.E{Key: "priority", Value: int32(3)}),
	)
	status := [][2]string{{"mongo-1:27017", "SECONDARY"}, {"mongo-2:27017", "PRIMARY"}}
	first := rsNode(t, &cfg, status...)
	primary := rsNode(t, &cfg, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": first,
		"10.0.0.2:27017": primary,
	})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017", "addr": "10.0.0.1:27017"},
			"b": map[string]any{"host": "mongo-2:27017", "addr": "10.0.0.2:27017"},
			"c": map[string]any{"host": "mongo-3:27017", "addr": "10.0.0.3:27017"},
		},
	})

	changed, _ := outcome(t, stream)
	if !changed {
		t.Fatal("completing a partial set must report changed=true")
	}
	if n := countCommand(first.calls, "replSetInitiate") + countCommand(primary.calls, "replSetInitiate"); n != 0 {
		t.Fatalf("replSetInitiate sent %d times at a set that already had a config", n)
	}

	// The reconfig went to the PRIMARY and nowhere else.
	if n := countCommand(first.calls, "replSetReconfig"); n != 0 {
		t.Errorf("replSetReconfig was sent to the SECONDARY at params.addr (%d times) — mongod only accepts it on the primary", n)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 1 {
		t.Fatalf("replSetReconfig reached the primary %d times, want exactly 1", n)
	}

	doc := reconfigDoc(t, primary.calls)

	if v, _ := docField(doc, "version"); v != int64(4) {
		t.Errorf("version = %v, want 4 (the live 3, plus one)", v)
	}
	if v, ok := docField(doc, "protocolVersion"); !ok || v != int64(1) {
		t.Errorf("protocolVersion = %v (present=%v) — a field this artifact does not model must ride through", v, ok)
	}
	settings, ok := docField(doc, "settings")
	if !ok {
		t.Fatal("the settings block was DROPPED — a config rebuilt from params is exactly what this must not be")
	}
	if got := canonicalValue(settings); got != canonicalValue(bson.D{
		{Key: "chainingAllowed", Value: false}, {Key: "heartbeatTimeoutSecs", Value: int32(7)},
	}) {
		t.Errorf("settings changed: %s", got)
	}

	members := configMembers(t, doc)
	if len(members) != 3 {
		t.Fatalf("config carries %d members, want 3", len(members))
	}
	// Existing members: identical documents, ids included.
	for _, want := range []struct {
		host string
		id   any
	}{{"mongo-1:27017", int32(0)}, {"mongo-2:27017", int32(5)}} {
		mem, ok := memberByHost(members, want.host)
		if !ok {
			t.Fatalf("member %s vanished from the config", want.host)
		}
		if id, _ := docField(mem, "_id"); id != want.id {
			t.Errorf("member %s got _id %v, want the live %v — an existing _id is never reassigned", want.host, id, want.id)
		}
	}
	if mem, _ := memberByHost(members, "mongo-2:27017"); mem != nil {
		if p, ok := docField(mem, "priority"); !ok || p != int32(3) {
			t.Errorf("the live priority of mongo-2 was rewritten to %v — an untouched member stays untouched", p)
		}
	}
	// The new one: max(_id)+1, not len(members).
	newMem, ok := memberByHost(members, "mongo-3:27017")
	if !ok {
		t.Fatal("the missing member was not added")
	}
	if id, _ := docField(newMem, "_id"); id != int64(6) {
		t.Errorf("the new member got _id %v, want 6 (max 5 + 1) — an index into the array would reuse a retired id", id)
	}
	if got := outputField(t, stream, "members_added"); got != float64(1) {
		t.Errorf("output.members_added = %v, want 1", got)
	}
}

// --- initiated: the refusals ---

// TestRSInitiated_NoReplicationEnabled_IsNotNotInitiated is the distinction an
// external `when: not initiated` probe cannot make: a mongod started WITHOUT
// replication.replSetName can never accept replSetInitiate, and driving it at one
// is a step that fails for a reason the operator was not told.
func TestRSInitiated_NoReplicationEnabled_IsNotNotInitiated(t *testing.T) {
	node := &fakeConn{}
	node.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		if cmd[0].Key == "replSetGetConfig" {
			return nil, cmdErr(codeNoReplicationEnabled, "NoReplicationEnabled")
		}
		return usersRaw(1), nil
	}
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs0",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017"}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "replSetName") {
		t.Errorf("the refusal must name the cause the operator has to fix, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetInitiate"); n != 0 {
		t.Errorf("replSetInitiate sent %d times at a mongod that is not in replica-set mode", n)
	}
}

// TestRSInitiated_ExtraLiveMember_IsRefusedNotDropped — a member the live config
// holds and params do not is refused by name. Dropping it silently is how a set
// loses its majority in a step the operator read as assembly.
func TestRSInitiated_ExtraLiveMember_IsRefusedNotDropped(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs0",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017"}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-2:27017") || !strings.Contains(msg, "member-removed") {
		t.Errorf("the refusal must name the member and the action that removes one, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused step still sent %d reconfig(s)", n)
	}
}

// TestRSInitiated_DriftedAttribute_IsRefusedNotSilentlyOk — the other half of
// additive-only. Reporting changed=false here would be a lie about convergence;
// rewriting the priority would be an election in an assembly step. The third
// answer, refusing and naming the action that does it deliberately, is the one.
func TestRSInitiated_DriftedAttribute_IsRefusedNotSilentlyOk(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017", bson.E{Key: "priority", Value: int32(1)}))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs0",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017", "priority": 5}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-1:27017") || !strings.Contains(msg, "reconfigured") {
		t.Errorf("the refusal must name the member and the action that changes one, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused step still sent %d reconfig(s)", n)
	}
}

// TestRSInitiated_MatchingAttributeIsNotDrift — the same comparison from the other
// side, across the numeric widths bson carries a number in. A live int32 priority
// of 1 against a declared 1 must not look like a change, or a converged set would
// be refused on every apply.
func TestRSInitiated_MatchingAttributeIsNotDrift(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017",
		bson.E{Key: "priority", Value: int32(1)}, bson.E{Key: "votes", Value: int32(1)}))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs0",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017", "priority": 1, "votes": 1}},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a matching attribute must not read as drift, got changed=true: %q", msg)
	}
}

// TestRSInitiated_NameMismatch_IsRefused — renaming a live replica set is not an
// operation, so a params.name that differs from the live `_id` is a refusal rather
// than a reconfig.
func TestRSInitiated_NameMismatch_IsRefused(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":    "10.0.0.1:27017",
		"name":    "rs-other",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017"}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "rs0") || !strings.Contains(msg, "rs-other") {
		t.Errorf("the refusal must name both the live and the declared set, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Error("a refused step still reconfigured the set")
	}
}

// TestRSInitiated_NoPrimaryWithinBudget_IsFailure — a set with no primary takes no
// writes, so reporting the step as reconciled would be false. Budget 0 reads once
// and fails, which is what keeps this test from waiting.
func TestRSInitiated_NoPrimaryWithinBudget_IsFailure(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "SECONDARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":                 "10.0.0.1:27017",
		"name":                 "rs0",
		"members":              map[string]any{"a": map[string]any{"host": "mongo-1:27017"}},
		"wait_primary_seconds": 0,
	})

	if msg := failureMessage(t, stream); !strings.Contains(msg, "PRIMARY") {
		t.Errorf("the failure must say a primary was never elected, got %q", msg)
	}
}

// TestRSInitiated_LocalhostExceptionBootstrap — a replica set is initiated BEFORE
// the first admin exists, so an auth failure on this action is the bootstrap case
// and must fall back to the no-auth loopback connection, exactly as `user.present`
// does.
func TestRSInitiated_LocalhostExceptionBootstrap(t *testing.T) {
	authed := &fakeConn{}
	authed.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		if cmd[0].Key == "usersInfo" {
			return nil, cmdErr(13, "Unauthorized")
		}
		return okRaw(), nil
	}
	noAuth := rsNode(t, nil, [2]string{"127.0.0.1:27017", "PRIMARY"})

	m := &MongoModule{connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
		if cfg.username == "" && cfg.password == "" {
			noAuth.cfg = cfg
			return noAuth, nil
		}
		authed.cfg = cfg
		return authed, nil
	}}

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":     "127.0.0.1:27017",
		"username": "default_admin",
		"password": secretPass,
		"name":     "rs0",
		"members":  map[string]any{"a": map[string]any{"host": "127.0.0.1:27017"}},
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Error("the bootstrap initiate must report changed=true")
	}
	if n := countCommand(noAuth.calls, "replSetInitiate"); n != 1 {
		t.Fatalf("replSetInitiate ran %d times on the no-auth connection, want 1", n)
	}
	if got := outputField(t, stream, "used_localhost"); got != true {
		t.Errorf("output.used_localhost = %v, want true", got)
	}
	assertEventsNoSecret(t, stream)
	if commandCarriesSecretOutsideCreateUser(noAuth.calls, secretPass) {
		t.Error("the password reached a command argument — it belongs in the connection only")
	}
}

// --- member-added ---

func TestRSMemberAdded_Idempotent_NoOp(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "10.0.0.1:27017",
		"member": map[string]any{"host": "mongo-2:27017"},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a host already in the config must be a no-op, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a no-op sent %d reconfig(s)", n)
	}
}

// TestRSMemberAdded_NewIdIsMaxPlusOne — a member removed earlier leaves a HOLE in
// the id sequence, and filling it would hand a new host the identity of an old
// one. The new id is max+1, never the array length.
func TestRSMemberAdded_NewIdIsMaxPlusOne(t *testing.T) {
	cfg := liveConfig(9, liveMember(0, "mongo-1:27017"), liveMember(7, "mongo-2:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"mongo-1:27017": node})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "mongo-1:27017",
		"member": map[string]any{"host": "mongo-3:27017", "priority": 0, "votes": 0, "hidden": true},
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Fatal("adding a member must report changed=true")
	}
	doc := reconfigDoc(t, node.calls)
	if v, _ := docField(doc, "version"); v != int64(10) {
		t.Errorf("version = %v, want 10", v)
	}
	mem, ok := memberByHost(configMembers(t, doc), "mongo-3:27017")
	if !ok {
		t.Fatal("the member was not added")
	}
	if id, _ := docField(mem, "_id"); id != int64(8) {
		t.Errorf("the new member got _id %v, want 8 (max 7 + 1)", id)
	}
	// Only the attributes the operator named are written.
	if h, ok := docField(mem, "hidden"); !ok || h != true {
		t.Errorf("hidden = %v (present=%v), want true", h, ok)
	}
	if _, ok := docField(mem, "buildIndexes"); ok {
		t.Error("an attribute the operator did not name was written into the config")
	}
}

// --- member-removed ---

func TestRSMemberRemoved_Idempotent_NoOp(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "10.0.0.1:27017",
		"host": "mongo-9:27017",
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a host that is not in the set must be a no-op, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a no-op sent %d reconfig(s)", n)
	}
}

// TestRSMemberRemoved_PrimaryIsRefused — removing the current primary by reconfig
// forces an election, so it is a refusal naming the command that steps one down.
func TestRSMemberRemoved_PrimaryIsRefused(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"}, [2]string{"mongo-2:27017", "SECONDARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "10.0.0.1:27017",
		"host": "mongo-1:27017",
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "PRIMARY") || !strings.Contains(msg, "replSetStepDown") {
		t.Errorf("the refusal must say it is the primary and how to hand over first, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused removal still sent %d reconfig(s)", n)
	}
}

// TestRSMemberRemoved_LastElectableIsRefused — a set left with no member that can
// win an election cannot elect a primary and so cannot take a write. Removing the
// last one is refused rather than performed and reported as success.
func TestRSMemberRemoved_LastElectableIsRefused(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(0)}, bson.E{Key: "votes", Value: int32(0)}),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-2:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "10.0.0.1:27017",
		"host": "mongo-1:27017",
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "electable") {
		t.Errorf("the refusal must say the set would have no electable member left, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused removal still sent %d reconfig(s)", n)
	}
}

// TestRSMemberRemoved_KeepsTheRest — the removal is the live config minus one
// member, so everything else, modelled or not, survives.
func TestRSMemberRemoved_KeepsTheRest(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017"),
		liveMember(2, "mongo-3:27017"),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"mongo-1:27017": node})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "mongo-1:27017",
		"host": "mongo-3:27017",
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Fatal("removing a member must report changed=true")
	}
	doc := reconfigDoc(t, node.calls)
	if _, ok := docField(doc, "settings"); !ok {
		t.Error("the settings block was dropped by a removal")
	}
	members := configMembers(t, doc)
	if len(members) != 2 {
		t.Fatalf("config carries %d members, want 2", len(members))
	}
	if _, ok := memberByHost(members, "mongo-3:27017"); ok {
		t.Error("the member was not removed")
	}
	for _, host := range []string{"mongo-1:27017", "mongo-2:27017"} {
		if _, ok := memberByHost(members, host); !ok {
			t.Errorf("member %s was removed too", host)
		}
	}
}

// --- reconfigured ---

// TestRSReconfigured_PatchesOnlyGivenAttrs — the action is a PATCH: a field the
// operator did not name keeps its live value, and a member not named is not
// touched at all.
func TestRSReconfigured_PatchesOnlyGivenAttrs(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017",
			bson.E{Key: "priority", Value: int32(1)},
			bson.E{Key: "votes", Value: int32(1)},
			bson.E{Key: "tags", Value: bson.D{{Key: "dc", Value: "east"}}}),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(2)}),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"mongo-1:27017": node})

	stream := rsApply(t, m, "reconfigured", map[string]any{
		"addr":    "mongo-1:27017",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017", "priority": 0}},
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Fatal("a real attribute change must report changed=true")
	}
	members := configMembers(t, reconfigDoc(t, node.calls))

	patched, _ := memberByHost(members, "mongo-1:27017")
	if p, _ := docField(patched, "priority"); p != float64(0) {
		t.Errorf("priority = %v, want 0", p)
	}
	if v, ok := docField(patched, "votes"); !ok || v != int32(1) {
		t.Errorf("votes = %v (present=%v) — an unnamed attribute keeps its live value", v, ok)
	}
	if tags, ok := docField(patched, "tags"); !ok || canonicalValue(tags) != canonicalValue(bson.D{{Key: "dc", Value: "east"}}) {
		t.Errorf("tags = %v (present=%v) — an unnamed attribute keeps its live value", tags, ok)
	}

	untouched, _ := memberByHost(members, "mongo-2:27017")
	if p, _ := docField(untouched, "priority"); p != int32(2) {
		t.Errorf("an unnamed MEMBER was changed: priority = %v, want the live 2", p)
	}
}

// TestRSReconfigured_Idempotent_NoOp — a run where every declared attribute already
// matches writes nothing. Without this the action would force an election on every
// apply, which is the worst possible thing for it to do.
func TestRSReconfigured_Idempotent_NoOp(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017",
		bson.E{Key: "priority", Value: int32(0)}, bson.E{Key: "hidden", Value: true}))
	node := rsNode(t, &cfg, [2]string{"mongo-2:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "reconfigured", map[string]any{
		"addr":    "10.0.0.1:27017",
		"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017", "priority": 0, "hidden": true}},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a converged reconfigure must be a no-op, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a no-op sent %d reconfig(s) — every one of them risks an election", n)
	}
}

// TestRSReconfigured_UnknownMemberIsRefused — this action only changes members that
// exist; joining one is member-added's job, and doing it here would make a "change"
// step grow the set.
func TestRSReconfigured_UnknownMemberIsRefused(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "reconfigured", map[string]any{
		"addr":    "10.0.0.1:27017",
		"members": map[string]any{"z": map[string]any{"host": "mongo-9:27017", "priority": 0}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-9:27017") || !strings.Contains(msg, "member-added") {
		t.Errorf("the refusal must name the member and the action that joins one, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Error("a refused reconfigure still wrote a config")
	}
}

// TestRSReconfigured_PrimaryAddrOverride — a set whose members address each other
// on a private network is managed from outside it: the primary's config host is not
// dialable here, and `primary_addr` is how the operator says where it is.
func TestRSReconfigured_PrimaryAddrOverride(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1.internal:27017", bson.E{Key: "priority", Value: int32(1)}))
	seed := rsNode(t, &cfg, [2]string{"mongo-1.internal:27017", "PRIMARY"})
	primary := rsNode(t, &cfg, [2]string{"mongo-1.internal:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"10.9.9.9:27017": primary,
	})

	stream := rsApply(t, m, "reconfigured", map[string]any{
		"addr":         "10.0.0.1:27017",
		"primary_addr": "10.9.9.9:27017",
		"members":      map[string]any{"a": map[string]any{"host": "mongo-1.internal:27017", "priority": 0}},
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Fatal("the reconfigure must report changed=true")
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 1 {
		t.Errorf("the reconfig reached the override address %d times, want 1", n)
	}
	if n := countCommand(seed.calls, "replSetReconfig"); n != 0 {
		t.Errorf("the reconfig also went to params.addr %d times", n)
	}
}

// --- Validate ---

// TestValidateRSInitiated_MemberRules — every constraint mongod enforces on a
// member document that is VISIBLE in the params is refused before the run, not in
// the middle of it (NIM-786).
func TestValidateRSInitiated_MemberRules(t *testing.T) {
	base := func(members map[string]any) map[string]any {
		return map[string]any{"addr": "127.0.0.1:27017", "name": "rs0", "members": members}
	}
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			"a hidden member that could win an election",
			base(map[string]any{"a": map[string]any{"host": "h1:27017", "hidden": true}}),
			"priority 0",
		},
		{
			"an arbiter with a priority",
			base(map[string]any{"a": map[string]any{"host": "h1:27017", "arbiter_only": true, "priority": 1}}),
			"priority 0",
		},
		{
			"a delayed member that could win an election",
			base(map[string]any{
				"a": map[string]any{"host": "h1:27017"},
				"b": map[string]any{"host": "h2:27017", "secondary_delay_secs": 3600},
			}),
			"priority 0",
		},
		{
			"a non-voting member with a priority",
			base(map[string]any{
				"a": map[string]any{"host": "h1:27017"},
				"b": map[string]any{"host": "h2:27017", "votes": 0, "priority": 1},
			}),
			"priority 0",
		},
		{
			"votes outside 0..1",
			base(map[string]any{"a": map[string]any{"host": "h1:27017", "votes": 2}}),
			"must be 0 or 1",
		},
		{
			"two entries on one host",
			base(map[string]any{
				"a": map[string]any{"host": "h1:27017"},
				"b": map[string]any{"host": "h1:27017"},
			}),
			"one member per host",
		},
		{
			"a set nobody can be elected in",
			base(map[string]any{"a": map[string]any{"host": "h1:27017", "priority": 0}}),
			"cannot take a write",
		},
		{
			"a member with no host",
			base(map[string]any{"a": map[string]any{"priority": 1}}),
			"host",
		},
		{
			"an empty membership",
			base(map[string]any{}),
			"non-empty map",
		},
		{
			"a negative election budget",
			map[string]any{"addr": "127.0.0.1:27017", "name": "rs0",
				"members": map[string]any{"a": map[string]any{"host": "h1:27017"}}, "wait_primary_seconds": -1},
			"must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateRSInitiated(mustFields(t, tc.params))
			if len(errs) == 0 {
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(strings.Join(errs, "; "), tc.wantErr) {
				t.Errorf("refusal %v does not mention %q", errs, tc.wantErr)
			}
		})
	}
}

// TestValidateRSInitiated_TooManyVoters — mongod caps a set at seven voting
// members, and the eighth is visible in the params.
func TestValidateRSInitiated_TooManyVoters(t *testing.T) {
	members := map[string]any{}
	for i := 0; i < 8; i++ {
		members[string(rune('a'+i))] = map[string]any{"host": string(rune('a'+i)) + ":27017"}
	}
	errs := validateRSInitiated(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "rs0", "members": members,
	}))
	if !strings.Contains(strings.Join(errs, "; "), "VOTING") {
		t.Errorf("expected the seven-voter cap to be refused, got %v", errs)
	}
}

// TestValidateRSInitiated_HappyPath — a well-formed set passes, or the rules above
// are refusing everything and proving nothing.
func TestValidateRSInitiated_HappyPath(t *testing.T) {
	errs := validateRSInitiated(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "h1:27017"},
			"b": map[string]any{"host": "h2:27017", "priority": 2},
			"c": map[string]any{"host": "h3:27017", "hidden": true, "priority": 0, "votes": 0},
		},
	}))
	if len(errs) != 0 {
		t.Errorf("a well-formed set was refused: %v", errs)
	}
}

// TestValidateRSReconfigured_DoesNotOverReject is the other half of NIM-786, and
// the direction that is easier to get wrong: `hidden: true` with no `priority`
// beside it is NOT an error on a patch, because the live config may already hold
// priority 0 and Validate cannot see it. Refusing here would refuse input Apply
// accepts.
func TestValidateRSReconfigured_DoesNotOverReject(t *testing.T) {
	fields := mustFields(t, map[string]any{
		"addr":    "127.0.0.1:27017",
		"members": map[string]any{"a": map[string]any{"host": "h1:27017", "hidden": true}},
	})
	if errs := validateRSReconfigured(fields); len(errs) != 0 {
		t.Errorf("a patch naming only `hidden` was refused: %v", errs)
	}
	// The same input on `initiated`, which writes a WHOLE member, IS refused:
	// there the omitted priority takes mongod's default of 1.
	whole := mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "rs0",
		"members": map[string]any{"a": map[string]any{"host": "h1:27017", "hidden": true}},
	})
	if errs := validateRSInitiated(whole); len(errs) == 0 {
		t.Error("`hidden` with no priority must be refused where the default applies")
	}
}

// TestValidateRSReconfigured_StillRefusesWhatIsDecidable — a patch is not a licence
// to skip every rule: `votes: 2` is wrong whatever the live config holds.
func TestValidateRSReconfigured_StillRefusesWhatIsDecidable(t *testing.T) {
	errs := validateRSReconfigured(mustFields(t, map[string]any{
		"addr":    "127.0.0.1:27017",
		"members": map[string]any{"a": map[string]any{"host": "h1:27017", "votes": 2}},
	}))
	if !strings.Contains(strings.Join(errs, "; "), "must be 0 or 1") {
		t.Errorf("expected votes to be refused on a patch too, got %v", errs)
	}
}

// TestParseMember_UnknownKeyIsRefused is the NIM-800 rule one level down. The
// engine's `unknown_param` stops at the outer map — nothing declares what is INSIDE
// `members` — so an unrecognised key used to be dropped and the member built
// without it. Three of the seven attributes are spelled differently here from the
// mongod config an author is reading, which makes `arbiterOnly` the likely typo:
// dropping it joins a full data-bearing secondary, which initial-syncs the whole
// dataset, where an arbiter was declared, and reports it reconciled.
func TestParseMember_UnknownKeyIsRefused(t *testing.T) {
	for _, key := range []string{"arbiterOnly", "buildIndexes", "slaveDelay", "prority"} {
		t.Run(key, func(t *testing.T) {
			node := rsNode(t, nil, [2]string{"h1:27017", "PRIMARY"})
			m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})
			stream := rsApply(t, m, "initiated", map[string]any{
				"addr": "10.0.0.1:27017", "name": "rs0",
				"members": map[string]any{"a": map[string]any{"host": "h1:27017", key: true}},
			})

			msg := failureMessage(t, stream)
			if !strings.Contains(msg, key) {
				t.Errorf("the refusal must name the unknown key, got %q", msg)
			}
			if n := countCommand(node.calls, "replSetInitiate"); n != 0 {
				t.Errorf("a member with an unrecognised key still reached replSetInitiate %d times", n)
			}
		})
	}
}

// TestValidateRS_ArbiterNeedsNoExplicitPriority — mongod defaults an arbiter's
// priority to 0, which is why `rs.addArb()` takes no priority argument. Defaulting
// it to 1 here made Validate refuse a member Apply accepts happily, since
// [memberDoc] writes only what was named. Over-refusing is the other half of
// NIM-786 and the easier half to ship by accident.
func TestValidateRS_ArbiterNeedsNoExplicitPriority(t *testing.T) {
	errs := validateRSMemberAdded(mustFields(t, map[string]any{
		"addr":   "127.0.0.1:27017",
		"member": map[string]any{"host": "arb:27017", "arbiter_only": true},
	}))
	if len(errs) != 0 {
		t.Errorf("an arbiter declared the way mongod documents it was refused: %v", errs)
	}

	// And the set-level rule must not count it as the member that can be elected.
	setErrs := validateRSInitiated(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "rs0",
		"members": map[string]any{"a": map[string]any{"host": "arb:27017", "arbiter_only": true}},
	}))
	if !strings.Contains(strings.Join(setErrs, "; "), "cannot take a write") {
		t.Errorf("a set of nothing but an arbiter must be refused, got %v", setErrs)
	}
}

// TestRSInitiated_ReorderedTagsAreNotDrift — mongod returns a member's tags in
// whatever order it stored them. A positional comparison failed `initiated`
// permanently on a member nothing was wrong with, and would have made
// `reconfigured` send a real reconfig — with its election risk — purely to reorder
// two keys.
func TestRSInitiated_ReorderedTagsAreNotDrift(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017", bson.E{Key: "tags", Value: bson.D{
		{Key: "zone", Value: "a"}, {Key: "dc", Value: "east"},
	}}))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017", "name": "rs0",
		"members": map[string]any{"a": map[string]any{
			"host": "mongo-1:27017",
			"tags": map[string]any{"dc": "east", "zone": "a"},
		}},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("tags that differ only in key order must not read as drift, got %q", msg)
	}

	// A tag that really differs is still caught.
	cfg2 := liveConfig(4, liveMember(0, "mongo-1:27017", bson.E{Key: "tags", Value: bson.D{
		{Key: "dc", Value: "west"},
	}}))
	node2 := rsNode(t, &cfg2, [2]string{"mongo-1:27017", "PRIMARY"})
	m2 := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node2})
	stream2 := rsApply(t, m2, "initiated", map[string]any{
		"addr": "10.0.0.1:27017", "name": "rs0",
		"members": map[string]any{"a": map[string]any{
			"host": "mongo-1:27017", "tags": map[string]any{"dc": "east"},
		}},
	})
	if msg := failureMessage(t, stream2); !strings.Contains(msg, "mongo-1:27017") {
		t.Errorf("a real tag change must still be caught, got %q", msg)
	}
}

// TestRSMemberAdded_DriftedAttributeIsRefused — `changed=false` has to be a fact
// about the DECLARATION, not about the hostname. Matching on the host alone let a
// member joined here as a hidden non-voter, later given priority 1 out of band,
// keep reporting "already in the set" while it had become able to win an election.
func TestRSMemberAdded_DriftedAttributeIsRefused(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(1)}, bson.E{Key: "hidden", Value: false}),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "10.0.0.1:27017",
		"member": map[string]any{"host": "mongo-2:27017", "hidden": true, "priority": 0},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-2:27017") || !strings.Contains(msg, "reconfigured") {
		t.Errorf("the refusal must name the member and the action that changes one, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused join still sent %d reconfig(s)", n)
	}
}

// TestRSMemberRemoved_EmptyHostIsRefusedOnTheApplyPath — the reverse direction of
// NIM-786, and the runtime calls Apply. An empty host matched no live member and
// came back "member  is not in the set (no-op)": success, changed=false, for a step
// that did nothing anyone asked for.
func TestRSMemberRemoved_EmptyHostIsRefusedOnTheApplyPath(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"))
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "10.0.0.1:27017", "host": "",
	})

	if msg := failureMessage(t, stream); !strings.Contains(msg, "params.host") {
		t.Errorf("the refusal must name the param, got %q", msg)
	}
	if len(node.calls) != 0 {
		t.Errorf("a refused step still reached mongod: %v", node.calls)
	}
}

// TestWaitForPrimary_NamesThePermissionCase — a step authenticating without
// clusterMonitor gets Unauthorized from every replSetGetStatus. Reporting "no
// PRIMARY elected" about a perfectly healthy set is a false statement that hides a
// one-line grant.
func TestWaitForPrimary_NamesThePermissionCase(t *testing.T) {
	conn := &fakeConn{}
	conn.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		if cmd[0].Key == "replSetGetStatus" {
			return nil, cmdErr(13, "Unauthorized")
		}
		return okRaw(), nil
	}
	_, err := waitForPrimary(context.Background(), conn, 0)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "Unauthorized") || !strings.Contains(err.Error(), "clusterMonitor") {
		t.Errorf("the failure must carry what replSetGetStatus actually said, got %q", err)
	}
}

// TestRSDay2_NoOpStillRequiresAPrimary — every day-2 no-op path waits for a
// primary, as its State Description promises. Without these three cases the wait
// could be deleted from all of them and the suite would stay green: the other
// no-op tests script a status that already reports a PRIMARY, so they cannot tell
// the difference.
//
// A set with no primary takes no writes, and reporting the converged case as fine
// while the changed case fails is the inconsistency that teaches an operator the
// report means nothing.
func TestRSDay2_NoOpStillRequiresAPrimary(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	cases := []struct {
		state  string
		params map[string]any
	}{
		{"member-added", map[string]any{"member": map[string]any{"host": "mongo-2:27017"}}},
		{"member-removed", map[string]any{"host": "mongo-9:27017"}},
		{"reconfigured", map[string]any{"members": map[string]any{"a": map[string]any{"host": "mongo-1:27017"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			// Every member SECONDARY: the set is live but cannot take a write.
			node := rsNode(t, &cfg,
				[2]string{"mongo-1:27017", "SECONDARY"}, [2]string{"mongo-2:27017", "SECONDARY"})
			m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

			params := map[string]any{"addr": "10.0.0.1:27017", "wait_primary_seconds": 0}
			for k, v := range tc.params {
				params[k] = v
			}
			stream := rsApply(t, m, tc.state, params)

			if msg := failureMessage(t, stream); !strings.Contains(msg, "PRIMARY") {
				t.Errorf("a no-op against a set with no primary must fail, got %q", msg)
			}
			if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
				t.Errorf("a failed no-op still wrote %d reconfig(s)", n)
			}
		})
	}
}

// TestRSInitiated_ServerErrorDoesNotLeakThePassword — the redaction guard with
// teeth. The other secret assertions on this object run on SUCCESS paths, whose
// event text never carries a driver error, so they pass on a build where every
// redactError call is replaced by err.Error(). This one makes the server return an
// error whose text EMBEDS the password, which is the shape user_test.go uses.
func TestRSInitiated_ServerErrorDoesNotLeakThePassword(t *testing.T) {
	node := &fakeConn{}
	node.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		switch cmd[0].Key {
		case "usersInfo":
			return usersRaw(1), nil
		case "replSetGetConfig":
			return nil, cmdErr(codeNotYetInitialized, "NotYetInitialized")
		case "replSetInitiate":
			// A driver error carrying the credential, as an auth/URI path can.
			return nil, fmt.Errorf("connection() error occurred during connection handshake: "+
				"auth error: sasl conversation error for user admin (password %s)", secretPass)
		}
		return okRaw(), nil
	}
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr":     "10.0.0.1:27017",
		"username": "default_admin",
		"password": secretPass,
		"name":     "rs0",
		"members":  map[string]any{"a": map[string]any{"host": "mongo-1:27017"}},
	})

	msg := failureMessage(t, stream)
	if strings.Contains(msg, secretPass) {
		t.Errorf("the password reached the failure message: %q", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Errorf("the message should carry the redaction marker, got %q", msg)
	}
	assertEventsNoSecret(t, stream)
}

// TestRSMemberAdded_NoOpComparesTheDeclaration — the sibling idempotency test
// declares a member with NO attributes, so `given` is empty and the comparison
// cannot fail; it would pass on a build matching on host alone. This one declares
// attributes that MATCH and asserts the no-op, so the comparison is exercised in
// the direction that must succeed, next to the refusal case that must not.
func TestRSMemberAdded_NoOpComparesTheDeclaration(t *testing.T) {
	cfg := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(0)}, bson.E{Key: "hidden", Value: true}),
	)
	node := rsNode(t, &cfg, [2]string{"mongo-1:27017", "PRIMARY"})
	m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": node})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "10.0.0.1:27017",
		"member": map[string]any{"host": "mongo-2:27017", "priority": 0, "hidden": true},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a member already matching its declaration must be a no-op, got %q", msg)
	}
	if n := countCommand(node.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a no-op wrote %d reconfig(s)", n)
	}
	if got := outputField(t, stream, "primary"); got != "mongo-1:27017" {
		t.Errorf("output.primary = %v — the converged run must publish it too, or a downstream "+
			"${ register.*.primary } breaks on exactly the run that changed nothing", got)
	}
}

// rsNodeCfg scripts a node whose config is its OWN, so a test can give the seed and
// the primary DIFFERENT documents. Every other two-node test scripts both from one
// `cfg`, which is exactly why none of them can see whether the re-read happens.
func rsNodeCfg(t *testing.T, cfg bson.D, status ...[2]string) *fakeConn {
	t.Helper()
	return rsNode(t, &cfg, status...)
}

// TestRSMemberAdded_UsesThePrimarysConfigNotTheSeeds is the guard for the whole
// primary-hop re-read. The seed is a config version behind and does not hold
// mongo-3; the PRIMARY already does. Deriving the reconfig from the seed would send
// version 5 at a primary holding 5 — mongod refuses it with "must be greater than
// the current one" — and would add a host the set already has.
func TestRSMemberAdded_UsesThePrimarysConfigNotTheSeeds(t *testing.T) {
	stale := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	fresh := liveConfig(5, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"),
		liveMember(2, "mongo-3:27017"))
	status := [][2]string{{"mongo-1:27017", "SECONDARY"}, {"mongo-2:27017", "PRIMARY"}}

	seed := rsNodeCfg(t, stale, status...)
	primary := rsNodeCfg(t, fresh, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-2:27017":  primary,
	})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "10.0.0.1:27017",
		"member": map[string]any{"host": "mongo-3:27017"},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("the primary already holds mongo-3 — deriving from the SEED's config would add it twice, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a member the primary already has was re-added (%d reconfig(s))", n)
	}
	if got := outputField(t, stream, "version"); got != float64(5) {
		t.Errorf("output.version = %v, want the PRIMARY's 5 — the seed's is 4", got)
	}
}

// TestRSMemberAdded_PrimarySideDriftIsRefused — the member appeared on the primary
// between the two reads, but NOT as declared. The seed-side gate cannot see it (the
// member was absent there), so matching on the host alone at the primary would
// report a member able to win an election as reconciled with a declaration that
// says it cannot.
func TestRSMemberAdded_PrimarySideDriftIsRefused(t *testing.T) {
	stale := liveConfig(4, liveMember(0, "mongo-1:27017"))
	fresh := liveConfig(5, liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(1)}, bson.E{Key: "hidden", Value: false}))
	status := [][2]string{{"mongo-1:27017", "PRIMARY"}}

	seed := rsNodeCfg(t, stale, status...)
	primary := rsNodeCfg(t, fresh, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-1:27017":  primary,
	})

	stream := rsApply(t, m, "member-added", map[string]any{
		"addr":   "10.0.0.1:27017",
		"member": map[string]any{"host": "mongo-2:27017", "priority": 0, "hidden": true},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-2:27017") || !strings.Contains(msg, "reconfigured") {
		t.Errorf("the refusal must name the member and the action that changes one, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused join still wrote %d reconfig(s)", n)
	}
}

// TestRSInitiated_PrimarySideDriftIsRefused — the same class on `initiated`: a
// declared member that was MISSING on the seed and has since arrived on the primary
// with different attributes. Skipping it silently would report the set formed and
// converged while it holds an electable voter the declaration forbids.
func TestRSInitiated_PrimarySideDriftIsRefused(t *testing.T) {
	stale := liveConfig(4, liveMember(0, "mongo-1:27017"))
	fresh := liveConfig(5, liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "votes", Value: int32(1)}, bson.E{Key: "priority", Value: int32(1)}))
	status := [][2]string{{"mongo-1:27017", "PRIMARY"}}

	seed := rsNodeCfg(t, stale, status...)
	primary := rsNodeCfg(t, fresh, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-1:27017":  primary,
	})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017", "votes": 0, "priority": 0},
		},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-2:27017") || !strings.Contains(msg, "reconfigured") {
		t.Errorf("the refusal must name the member and the action that changes one, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a refused step still wrote %d reconfig(s)", n)
	}
}

// TestRSMemberRemoved_PrimaryMovedIsRefused — the election runs on its own schedule,
// so the node the seed named can have handed over by the time the hop connects.
// [MongoModule.primaryView] confirms the connection is STILL the primary and refuses
// otherwise, which covers all four actions at once: a replSetReconfig down a
// secondary comes back as NotWritablePrimary, the opaque failure the hop exists to
// avoid. Here it also covers the case this action names by hand — the member being
// removed becoming the primary.
func TestRSMemberRemoved_PrimaryMovedIsRefused(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))

	// The seed still believes mongo-1 leads; the primary reports mongo-2 does —
	// and mongo-2 is the host being removed.
	seed := rsNodeCfg(t, cfg, [2]string{"mongo-1:27017", "PRIMARY"}, [2]string{"mongo-2:27017", "SECONDARY"})
	primary := rsNodeCfg(t, cfg, [2]string{"mongo-2:27017", "PRIMARY"}, [2]string{"mongo-1:27017", "SECONDARY"})
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-1:27017":  primary,
	})

	stream := rsApply(t, m, "member-removed", map[string]any{
		"addr": "10.0.0.1:27017",
		"host": "mongo-2:27017",
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "PRIMARY moved") || !strings.Contains(msg, "nothing was written") {
		t.Errorf("the refusal must name the move and say nothing was written, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a reconfig was sent to a node that is no longer the primary (%d)", n)
	}
}

// TestRSPrimaryHopRefusesAStaleTargetOnEveryAction — the confirmation lives in
// primaryView, so it covers all four actions rather than the one that noticed it.
// The seed names mongo-1; by the time the hop connects, mongo-2 leads.
func TestRSPrimaryHopRefusesAStaleTargetOnEveryAction(t *testing.T) {
	cfg := liveConfig(4, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-2:27017"))
	cases := []struct {
		state  string
		params map[string]any
	}{
		{"initiated", map[string]any{"name": "rs0", "members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017"},
			"c": map[string]any{"host": "mongo-3:27017"},
		}}},
		{"member-added", map[string]any{"member": map[string]any{"host": "mongo-9:27017"}}},
		{"member-removed", map[string]any{"host": "mongo-2:27017"}},
		{"reconfigured", map[string]any{"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017", "priority": 0}}}},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			seed := rsNodeCfg(t, cfg, [2]string{"mongo-1:27017", "PRIMARY"}, [2]string{"mongo-2:27017", "SECONDARY"})
			hop := rsNodeCfg(t, cfg, [2]string{"mongo-2:27017", "PRIMARY"}, [2]string{"mongo-1:27017", "SECONDARY"})
			m := newModuleByAddr(map[string]*fakeConn{"10.0.0.1:27017": seed, "mongo-1:27017": hop})

			params := map[string]any{"addr": "10.0.0.1:27017"}
			for k, v := range tc.params {
				params[k] = v
			}
			stream := rsApply(t, m, tc.state, params)

			if msg := failureMessage(t, stream); !strings.Contains(msg, "PRIMARY moved") {
				t.Errorf("the stale hop target must be refused, got %q", msg)
			}
			if n := countCommand(hop.calls, "replSetReconfig"); n != 0 {
				t.Errorf("a reconfig reached a node that is no longer the primary (%d)", n)
			}
		})
	}
}

// TestRSInitiated_SeedMatchedMemberDriftIsCaughtOnThePrimary — the re-classification
// against the primary's document covers members the SEED called matching, not only
// the ones it called missing. Checking only the missing half left the larger half
// unguarded: a member that matched on the seed and was changed out of band a moment
// later would be copied out of the primary's document into the reconfig, and the set
// reported as completed while it holds what the declaration forbids.
func TestRSInitiated_SeedMatchedMemberDriftIsCaughtOnThePrimary(t *testing.T) {
	// Seed: mongo-2 matches the declaration (priority 0), mongo-3 is missing.
	stale := liveConfig(4,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(0)}),
	)
	// Primary: mongo-3 arrived, and mongo-2 was given priority 1 out of band.
	fresh := liveConfig(5,
		liveMember(0, "mongo-1:27017"),
		liveMember(1, "mongo-2:27017", bson.E{Key: "priority", Value: int32(1)}),
		liveMember(2, "mongo-3:27017"),
	)
	status := [][2]string{{"mongo-1:27017", "PRIMARY"}}

	seed := rsNodeCfg(t, stale, status...)
	primary := rsNodeCfg(t, fresh, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-1:27017":  primary,
	})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017", "priority": 0},
			"c": map[string]any{"host": "mongo-3:27017"},
		},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-2:27017") || !strings.Contains(msg, "reconfigured") {
		t.Errorf("the refusal must name the drifted member and the action that changes one, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a reconfig was written carrying the drifted member (%d)", n)
	}
}

// TestRSInitiated_ExtraMemberOnThePrimaryIsRefused — the same re-classification
// catches a member that appeared on the primary and is not declared at all. The
// seed-side gate cannot see it; dropping it silently is how a set loses its majority.
func TestRSInitiated_ExtraMemberOnThePrimaryIsRefused(t *testing.T) {
	stale := liveConfig(4, liveMember(0, "mongo-1:27017"))
	fresh := liveConfig(5, liveMember(0, "mongo-1:27017"), liveMember(1, "mongo-9:27017"))
	status := [][2]string{{"mongo-1:27017", "PRIMARY"}}

	seed := rsNodeCfg(t, stale, status...)
	primary := rsNodeCfg(t, fresh, status...)
	m := newModuleByAddr(map[string]*fakeConn{
		"10.0.0.1:27017": seed,
		"mongo-1:27017":  primary,
	})

	stream := rsApply(t, m, "initiated", map[string]any{
		"addr": "10.0.0.1:27017",
		"name": "rs0",
		"members": map[string]any{
			"a": map[string]any{"host": "mongo-1:27017"},
			"b": map[string]any{"host": "mongo-2:27017"},
		},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "mongo-9:27017") || !strings.Contains(msg, "member-removed") {
		t.Errorf("the refusal must name the undeclared member and the action that removes one, got %q", msg)
	}
	if n := countCommand(primary.calls, "replSetReconfig"); n != 0 {
		t.Errorf("a reconfig was written that would have dropped it (%d)", n)
	}
}
