// Guards on the `index` object. Two of these are the file's reason to exist.
//
// The first is ORDER: an index on { user_id: 1, created_at: -1 } is a different
// index from one on { created_at: -1, user_id: 1 }, and the comparison must say so.
// A key declared as a map, or compared through [canonicalValue] (which sorts map
// keys), would call those two the same and report a wrong index as converged.
//
// The second is that an index CANNOT BE MODIFIED: a declared key or immutable
// option that differs is a refusal, not a silent drop-and-rebuild — during that
// rebuild the queries that relied on the index have none.
package main

import (
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
)

// indexNode scripts listIndexes for one collection. live nil means the index is not
// there; missingNS makes the collection itself absent.
func indexNode(t *testing.T, live *bson.D, missingNS bool) *fakeConn {
	t.Helper()
	c := &fakeConn{}
	c.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		if cmd[0].Key == "listIndexes" {
			switch {
			case missingNS:
				return nil, cmdErr(codeNamespaceNotFound, "NamespaceNotFound")
			case live == nil:
				return listReply(t, bson.D{
					{Key: "name", Value: "_id_"},
					{Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}},
				}), nil
			default:
				return listReply(t, *live), nil
			}
		}
		return okRaw(), nil
	}
	return c
}

func indexApply(t *testing.T, m *MongoModule, state string, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := m.index().Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	return stream
}

// compoundParams declares a two-field key in a fixed order.
func compoundParams() map[string]any {
	return map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "by_user_time",
		"keys": []any{
			map[string]any{"field": "user_id", "order": 1},
			map[string]any{"field": "created_at", "order": -1},
		},
	}
}

// liveCompound is the same key as mongod stores it, in the same order.
func liveCompound(extra ...bson.E) bson.D {
	return append(bson.D{
		{Key: "v", Value: int32(2)},
		{Key: "name", Value: "by_user_time"},
		{Key: "key", Value: bson.D{
			{Key: "user_id", Value: int32(1)},
			{Key: "created_at", Value: int32(-1)},
		}},
	}, extra...)
}

func TestIndexPresent_CreatesWithTheDeclaredKeyOrder(t *testing.T) {
	conn := indexNode(t, nil, false)
	stream := indexApply(t, newModule(conn), "present", compoundParams())

	if changed, _ := outcome(t, stream); !changed {
		t.Error("creating an index must report changed=true")
	}
	call, ok := lastCommand(conn.calls, "createIndexes")
	if !ok {
		t.Fatal("no createIndexes was sent")
	}
	specs, ok := commandField(conn.calls, "createIndexes", "indexes")
	if !ok {
		t.Fatal("createIndexes carried no indexes")
	}
	arr, _ := specs.(bson.A)
	if len(arr) != 1 {
		t.Fatalf("createIndexes carried %d specs, want 1", len(arr))
	}
	spec, _ := arr[0].(bson.D)
	key, _ := docField(spec, "key")
	keyDoc, ok := key.(bson.D)
	if !ok {
		t.Fatalf("key is %T, want an ORDERED bson.D", key)
	}
	if len(keyDoc) != 2 || keyDoc[0].Key != "user_id" || keyDoc[1].Key != "created_at" {
		t.Errorf("the key lost its declared order: %v", keyDoc)
	}
	if call.db != "appdb" {
		t.Errorf("createIndexes ran in %q, want appdb", call.db)
	}
}

func TestIndexPresent_NoOpWhenTheIndexMatches(t *testing.T) {
	live := liveCompound()
	conn := indexNode(t, &live, false)
	stream := indexApply(t, newModule(conn), "present", compoundParams())

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a matching index must be a no-op, got %q", msg)
	}
	if n := countCommand(conn.calls, "createIndexes") + countCommand(conn.calls, "collMod"); n != 0 {
		t.Errorf("a no-op wrote %d time(s)", n)
	}
}

// TestIndexPresent_ReversedCompoundKeyIsADifferentIndex is THE guard of this file.
// The live index has the same two fields with the same directions in the OTHER
// order — a genuinely different index — and it must be refused, not reported as
// converged.
func TestIndexPresent_ReversedCompoundKeyIsADifferentIndex(t *testing.T) {
	live := bson.D{
		{Key: "name", Value: "by_user_time"},
		{Key: "key", Value: bson.D{
			{Key: "created_at", Value: int32(-1)},
			{Key: "user_id", Value: int32(1)},
		}},
	}
	conn := indexNode(t, &live, false)
	stream := indexApply(t, newModule(conn), "present", compoundParams())

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "different key") {
		t.Errorf("a reordered compound key must be refused as a different index, got %q", msg)
	}
	if n := countCommand(conn.calls, "createIndexes") + countCommand(conn.calls, "dropIndexes"); n != 0 {
		t.Errorf("a refused step still wrote %d time(s) — a silent rebuild is what this avoids", n)
	}
}

// TestIndexKeyString_OrderIsMeaning is the same fact one level down, so a
// regression in the comparator is caught here rather than only through Apply.
func TestIndexKeyString_OrderIsMeaning(t *testing.T) {
	ab := keyString([]indexKeyPart{{field: "a", order: int64(1)}, {field: "b", order: int64(-1)}})
	ba := keyString([]indexKeyPart{{field: "b", order: int64(-1)}, {field: "a", order: int64(1)}})
	if ab == ba {
		t.Fatalf("two differently ordered keys rendered identically: %s", ab)
	}
	// And canonicalValue, which sorts, would have called them equal — which is
	// exactly why the key does not go through it.
	if canonicalValue(map[string]any{"a": int64(1), "b": int64(-1)}) !=
		canonicalValue(map[string]any{"b": int64(-1), "a": int64(1)}) {
		t.Error("canonicalValue no longer sorts map keys — the reason the index key does NOT go through it " +
			"has changed, and the comment above index.go's keyString is now wrong")
	}
}

// TestIndexPresent_ImmutableOptionDriftIsRefused — `unique` cannot be turned on or
// off on a live index.
func TestIndexPresent_ImmutableOptionDriftIsRefused(t *testing.T) {
	live := liveCompound()
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["unique"] = true
	stream := indexApply(t, newModule(conn), "present", params)

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "unique") || !strings.Contains(msg, "cannot be changed on a live index") {
		t.Errorf("the refusal must name the field and why, got %q", msg)
	}
	if n := countCommand(conn.calls, "dropIndexes"); n != 0 {
		t.Error("a refused step dropped the index")
	}
}

// TestIndexPresent_MutableOptionGoesThroughCollMod — a TTL and `hidden` ARE changed
// in place, which is the whole of what can be.
func TestIndexPresent_MutableOptionGoesThroughCollMod(t *testing.T) {
	live := liveCompound(bson.E{Key: "expireAfterSeconds", Value: int32(3600)})
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["expire_after_seconds"] = 604800
	stream := indexApply(t, newModule(conn), "present", params)

	if changed, _ := outcome(t, stream); !changed {
		t.Error("a TTL change must report changed=true")
	}
	if n := countCommand(conn.calls, "collMod"); n != 1 {
		t.Fatalf("collMod sent %d times, want 1", n)
	}
	spec, ok := commandField(conn.calls, "collMod", "index")
	if !ok {
		t.Fatal("collMod carried no index spec")
	}
	d, _ := spec.(bson.D)
	if v, _ := docField(d, "expireAfterSeconds"); v != int64(604800) {
		t.Errorf("collMod carried expireAfterSeconds=%v, want 604800", v)
	}
	if v, _ := docField(d, "name"); v != "by_user_time" {
		t.Errorf("collMod addressed index %v, want by_user_time", v)
	}
}

// TestIndexPresent_UnchangedTTLIsNotAChange — the same comparison from the other
// side, across the numeric widths bson carries a number in.
func TestIndexPresent_UnchangedTTLIsNotAChange(t *testing.T) {
	live := liveCompound(bson.E{Key: "expireAfterSeconds", Value: int32(604800)})
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["expire_after_seconds"] = 604800
	stream := indexApply(t, newModule(conn), "present", params)

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("an unchanged TTL must be a no-op, got %q", msg)
	}
}

func TestIndexAbsent_DropsAndIsIdempotent(t *testing.T) {
	t.Run("drops an existing index", func(t *testing.T) {
		live := liveCompound()
		conn := indexNode(t, &live, false)
		stream := indexApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "by_user_time",
		})
		if changed, _ := outcome(t, stream); !changed {
			t.Error("dropping an existing index must report changed=true")
		}
		if n := countCommand(conn.calls, "dropIndexes"); n != 1 {
			t.Errorf("dropIndexes sent %d times, want 1", n)
		}
	})

	t.Run("an index that is not there is a no-op", func(t *testing.T) {
		conn := indexNode(t, nil, false)
		stream := indexApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "by_user_time",
		})
		if changed, msg := outcome(t, stream); changed {
			t.Errorf("expected a no-op, got %q", msg)
		}
		if n := countCommand(conn.calls, "dropIndexes"); n != 0 {
			t.Errorf("a no-op sent dropIndexes %d times", n)
		}
	})

	// A collection that does not exist has no index either. NamespaceNotFound is
	// that answer, not a failure — without this the teardown half of a scenario
	// reds whenever the collection is already gone.
	t.Run("a collection that is not there is a no-op", func(t *testing.T) {
		conn := indexNode(t, nil, true)
		stream := indexApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "database": "appdb", "collection": "gone", "name": "by_user_time",
		})
		if changed, msg := outcome(t, stream); changed {
			t.Errorf("expected a no-op on a missing collection, got %q", msg)
		}
		if n := countCommand(conn.calls, "dropIndexes"); n != 0 {
			t.Errorf("a no-op sent dropIndexes %d times", n)
		}
	})
}

// TestValidateIndex_RefusesWhatApplyWould — [parseIndexKeys] is the same function
// both call (NIM-786).
func TestValidateIndex_RefusesWhatApplyWould(t *testing.T) {
	base := map[string]any{"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			"the index mongod maintains itself",
			with(map[string]any{"name": "_id_", "keys": []any{map[string]any{"field": "_id", "order": 1}}}),
			"_id_",
		},
		{
			"an empty key",
			with(map[string]any{"name": "i", "keys": []any{}}),
			"non-empty list",
		},
		{
			"a direction that is neither 1 nor -1",
			with(map[string]any{"name": "i", "keys": []any{map[string]any{"field": "a", "order": 2}}}),
			"1 (ascending) or -1",
		},
		{
			"a direction of the wrong type",
			with(map[string]any{"name": "i", "keys": []any{map[string]any{"field": "a", "order": true}}}),
			"must be 1, -1, or an index type",
		},
		{
			"a field indexed twice",
			with(map[string]any{"name": "i", "keys": []any{
				map[string]any{"field": "a", "order": 1},
				map[string]any{"field": "a", "order": -1},
			}}),
			"appears twice",
		},
		{
			"a key entry with no field",
			with(map[string]any{"name": "i", "keys": []any{map[string]any{"order": 1}}}),
			"non-empty document field path",
		},
		{
			"a negative TTL",
			with(map[string]any{"name": "i", "keys": []any{map[string]any{"field": "a", "order": 1}},
				"expire_after_seconds": -5}),
			"must be >= 0",
		},
		{
			"no collection",
			map[string]any{"addr": "127.0.0.1:27017", "database": "appdb", "name": "i",
				"keys": []any{map[string]any{"field": "a", "order": 1}}},
			"params.collection",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIndexPresent(mustFields(t, tc.params))
			if len(errs) == 0 {
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(strings.Join(errs, "; "), tc.wantErr) {
				t.Errorf("refusal %v does not mention %q", errs, tc.wantErr)
			}
		})
	}
}

// TestValidateIndexPresent_HappyPath — including a string index type, which is a
// legal key value and must not be caught by the direction rule.
func TestValidateIndexPresent_HappyPath(t *testing.T) {
	errs := validateIndexPresent(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "search",
		"keys": []any{
			map[string]any{"field": "location", "order": "2dsphere"},
			map[string]any{"field": "created_at"}, // order omitted -> ascending
		},
	}))
	if len(errs) != 0 {
		t.Errorf("a well-formed index was refused: %v", errs)
	}
}

// TestIndexPresent_TextKeyIsRefused — mongod stores a text index under a REWRITTEN
// key ({_fts, _ftsx}, the declared fields moved into `weights`), so a declared
// {title: "text"} can never match what listIndexes reads back: the first apply
// would create it and every apply after would refuse it as "a different key" and
// tell the operator to drop the index this artifact had just built. Refusing the
// declaration is the honest answer; a promise that cannot be kept is worse than an
// absent feature.
func TestIndexPresent_TextKeyIsRefused(t *testing.T) {
	conn := indexNode(t, nil, false)
	stream := indexApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "search",
		"keys": []any{map[string]any{"field": "title", "order": "text"}},
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "TEXT") || !strings.Contains(msg, "command.run") {
		t.Errorf("the refusal must name what is unsupported and what to use instead, got %q", msg)
	}
	if n := countCommand(conn.calls, "createIndexes"); n != 0 {
		t.Errorf("a refused text index still reached createIndexes %d times", n)
	}
}

// TestIndexPresent_AddingATTLIsNotACollMod — `collMod {index: {name,
// expireAfterSeconds}}` changes an EXISTING TTL; against an index that has none it
// answers InvalidOptions in the middle of a run, and re-running does not help. So
// adding one is refused before anything is sent, like any other rebuild.
func TestIndexPresent_AddingATTLIsNotACollMod(t *testing.T) {
	live := liveCompound() // no expireAfterSeconds
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["expire_after_seconds"] = 604800
	stream := indexApply(t, newModule(conn), "present", params)

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "expire_after_seconds") {
		t.Errorf("the refusal must name the field, got %q", msg)
	}
	if n := countCommand(conn.calls, "collMod"); n != 0 {
		t.Errorf("collMod was sent %d times at an index with no TTL — mongod answers InvalidOptions to that", n)
	}
}

// TestIndexPresent_ZeroTTLIsADeclarationNotADefault — `expire_after_seconds: 0` is
// a documented, meaningful value: expire at the indexed date itself. Treating a
// zero as "the default mongod omits" made a plain index report
// `changed=false, already present as declared` while it was not a TTL index at all
// — an idempotency lie from a rule meant to prevent a cosmetic one.
func TestIndexPresent_ZeroTTLIsADeclarationNotADefault(t *testing.T) {
	live := liveCompound() // no expireAfterSeconds
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["expire_after_seconds"] = 0
	stream := indexApply(t, newModule(conn), "present", params)

	if fin := stream.final(); fin != nil && !fin.GetFailed() && !fin.GetChanged() {
		t.Fatalf("a declared TTL of 0 on an index that has NO TTL was reported as converged: %q", fin.GetMessage())
	}
	if msg := failureMessage(t, stream); !strings.Contains(msg, "expire_after_seconds") {
		t.Errorf("the refusal must name the field, got %q", msg)
	}
}

// TestIndexPresent_FalseOptionAgainstAStoredNothingIsStillANoOp guards the OTHER
// direction of the narrowing: mongod really does omit `unique: false`, so the
// exemption had to survive it. It does NOT discriminate the narrowing itself —
// TestIndexPresent_ZeroTTLIsADeclarationNotADefault is what does.
func TestIndexPresent_FalseOptionAgainstAStoredNothingIsStillANoOp(t *testing.T) {
	live := liveCompound()
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["unique"] = false
	stream := indexApply(t, newModule(conn), "present", params)

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a declared false against a stored nothing must be a no-op, got %q", msg)
	}
}

// TestIndex_IdIndexIsRefusedOnTheApplyPathToo — `_id_` can be neither created nor
// dropped, and a runner need not call Validate. Without the refusal on this path
// `index.present name=_id_` reported a cheerful no-op and `index.absent name=_id_`
// sent dropIndexes for mongod to reject.
func TestIndex_IdIndexIsRefusedOnTheApplyPathToo(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			conn := indexNode(t, nil, false)
			stream := indexApply(t, newModule(conn), state, map[string]any{
				"addr": "127.0.0.1:27017", "database": "appdb", "collection": "events", "name": "_id_",
				"keys": []any{map[string]any{"field": "_id", "order": 1}},
			})
			if msg := failureMessage(t, stream); !strings.Contains(msg, "_id_") {
				t.Errorf("the refusal must name the index, got %q", msg)
			}
			if len(conn.calls) != 0 {
				t.Errorf("a refused step still reached mongod: %v", conn.calls)
			}
		})
	}
}

// TestIndexProbe_UnreadableReplyIsAFailureNotAbsence — an unparseable listIndexes
// decoded as "no such index" makes `absent` report a no-op about an index still
// there, and `present` re-issue createIndexes against one that exists.
func TestIndexProbe_UnreadableReplyIsAFailureNotAbsence(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			conn := &fakeConn{}
			conn.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
				if cmd[0].Key == "listIndexes" {
					return rawDoc(t, bson.D{{Key: "ok", Value: int32(1)}}), nil // no cursor
				}
				return okRaw(), nil
			}
			stream := indexApply(t, newModule(conn), state, compoundParams())
			if msg := failureMessage(t, stream); !strings.Contains(msg, "listIndexes") {
				t.Errorf("an unreadable probe reply must fail, got %q", msg)
			}
			for _, write := range []string{"createIndexes", "dropIndexes", "collMod"} {
				if n := countCommand(conn.calls, write); n != 0 {
					t.Errorf("%s was sent %d times after an unreadable probe", write, n)
				}
			}
		})
	}
}

// TestIndexPresent_SimpleCollationIsStoredAsNothing — "simple" is the binary
// collator and mongod stores no collation field for it; without the exemption the
// absent live value reads as an immutable drift on an index this step created.
func TestIndexPresent_SimpleCollationIsStoredAsNothing(t *testing.T) {
	live := liveCompound()
	conn := indexNode(t, &live, false)
	params := compoundParams()
	params["collation"] = map[string]any{"locale": "simple"}
	stream := indexApply(t, newModule(conn), "present", params)

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("collation simple stores nothing and must not read as drift, got %q", msg)
	}
}
