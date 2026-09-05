// Shared fakes for the NIM-805 objects: building bson replies, scripting a
// connection whose answers change across calls, and giving a module more than one
// node to dial.
//
// The last one exists because `replicaset` is the first object of this artifact
// that talks to a host other than `params.addr` — replSetReconfig runs on the
// PRIMARY — and a single-connection fake cannot tell a test whether it did.
package main

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// rawDoc marshals a document into the bson.Raw a fake connection replies with.
func rawDoc(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatalf("bson.Marshal: %v", err)
	}
	return b
}

// cmdErr is a typed mongod command error, which is what the code branches on
// before it falls back to matching the codeName in the text.
func cmdErr(code int32, name string) error {
	return mongo.CommandError{Code: code, Name: name, Message: name}
}

// rsConfigReply is a replSetGetConfig reply carrying cfg.
func rsConfigReply(t *testing.T, cfg bson.D) bson.Raw {
	t.Helper()
	return rawDoc(t, bson.D{{Key: "config", Value: cfg}, {Key: "ok", Value: int32(1)}})
}

// rsStatusReply is a replSetGetStatus reply: pairs of host and stateStr.
func rsStatusReply(t *testing.T, members ...[2]string) bson.Raw {
	t.Helper()
	arr := bson.A{}
	for _, m := range members {
		arr = append(arr, bson.D{{Key: "name", Value: m[0]}, {Key: "stateStr", Value: m[1]}})
	}
	return rawDoc(t, bson.D{{Key: "members", Value: arr}, {Key: "ok", Value: int32(1)}})
}

// listReply is a `cursor.firstBatch` reply, the shape listCollections and
// listIndexes both answer in.
func listReply(t *testing.T, docs ...bson.D) bson.Raw {
	t.Helper()
	arr := bson.A{}
	for _, d := range docs {
		arr = append(arr, d)
	}
	return rawDoc(t, bson.D{
		{Key: "cursor", Value: bson.D{{Key: "firstBatch", Value: arr}}},
		{Key: "ok", Value: int32(1)},
	})
}

// dbListReply is a listDatabases reply with the given database names.
func dbListReply(t *testing.T, names ...string) bson.Raw {
	t.Helper()
	arr := bson.A{}
	for _, n := range names {
		arr = append(arr, bson.D{{Key: "name", Value: n}})
	}
	return rawDoc(t, bson.D{{Key: "databases", Value: arr}, {Key: "ok", Value: int32(1)}})
}

// newModuleByAddr builds a module that hands out a DIFFERENT fake per address, so
// a test can prove which node a command reached. An address nothing is registered
// for is a connection failure, which is what a real host that is not there does.
func newModuleByAddr(conns map[string]*fakeConn) *MongoModule {
	return &MongoModule{
		connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
			conn, ok := conns[cfg.addr]
			if !ok {
				return nil, cmdErr(6, "HostUnreachable")
			}
			conn.cfg = cfg
			return conn, nil
		},
	}
}

// countCommand is how many times a command with this first key was sent on this
// connection. Zero is an assertion in its own right: `replSetInitiate` must NEVER
// be sent at a set that already has a config.
func countCommand(calls []cmdCall, name string) int {
	n := 0
	for _, c := range calls {
		if len(c.cmd) > 0 && c.cmd[0].Key == name {
			n++
		}
	}
	return n
}

// lastCommand is the last call whose first key is name.
func lastCommand(calls []cmdCall, name string) (cmdCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if len(calls[i].cmd) > 0 && calls[i].cmd[0].Key == name {
			return calls[i], true
		}
	}
	return cmdCall{}, false
}

// reconfigDoc is the configuration document of the replSetReconfig that was sent.
func reconfigDoc(t *testing.T, calls []cmdCall) bson.D {
	t.Helper()
	call, ok := lastCommand(calls, "replSetReconfig")
	if !ok {
		t.Fatal("no replSetReconfig was sent")
	}
	doc, ok := call.cmd[0].Value.(bson.D)
	if !ok {
		t.Fatalf("replSetReconfig value is %T, want bson.D", call.cmd[0].Value)
	}
	return doc
}

// docField reads one top-level field of an ordered document.
func docField(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// configMembers reads the members array of a config document as documents.
func configMembers(t *testing.T, cfg bson.D) []bson.D {
	t.Helper()
	v, ok := docField(cfg, "members")
	if !ok {
		t.Fatal("config carries no members")
	}
	arr, ok := v.(bson.A)
	if !ok {
		t.Fatalf("members is %T, want bson.A", v)
	}
	out := make([]bson.D, 0, len(arr))
	for _, m := range arr {
		d, ok := m.(bson.D)
		if !ok {
			t.Fatalf("a member is %T, want bson.D", m)
		}
		out = append(out, d)
	}
	return out
}

// memberByHost finds a member document by its host.
func memberByHost(members []bson.D, host string) (bson.D, bool) {
	for _, m := range members {
		if h, ok := docField(m, "host"); ok && h == host {
			return m, true
		}
	}
	return nil, false
}

// failureMessage is the message of the final event, which must be a failure.
func failureMessage(t *testing.T, s *applyStream) string {
	t.Helper()
	fin := s.final()
	if fin == nil {
		t.Fatal("no event was sent")
	}
	if !fin.GetFailed() {
		t.Fatalf("expected failed=true, got %+v", fin)
	}
	return fin.GetMessage()
}

// outcome is the final event, which must NOT be a failure, plus its changed flag.
func outcome(t *testing.T, s *applyStream) (bool, string) {
	t.Helper()
	fin := s.final()
	if fin == nil {
		t.Fatal("no event was sent")
	}
	if fin.GetFailed() {
		t.Fatalf("expected success, got failure: %s", fin.GetMessage())
	}
	return fin.GetChanged(), fin.GetMessage()
}

// outputField reads one field of the final event's Output.
func outputField(t *testing.T, s *applyStream, key string) any {
	t.Helper()
	fin := s.final()
	if fin == nil || fin.GetOutput() == nil {
		t.Fatalf("final event carries no output: %+v", fin)
	}
	v, ok := fin.GetOutput().GetFields()[key]
	if !ok {
		t.Fatalf("output has no field %q: %v", key, fin.GetOutput().AsMap())
	}
	return v.AsInterface()
}
