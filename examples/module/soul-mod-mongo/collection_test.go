// Guards on the `collection` object. The two that matter are the mutable/immutable
// split — an immutable option that drifted must FAIL rather than silently no-op or
// silently rebuild — and the subset comparison for the options mongod stores with
// its own defaults filled in, without which a converged collation would read as an
// immutable drift and red the step forever.
package main

import (
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
)

// collNode scripts listCollections and listDatabases for one collection.
// liveOptions nil means the collection is not there.
func collNode(t *testing.T, liveOptions *bson.D, kind string, databases ...string) *fakeConn {
	t.Helper()
	c := &fakeConn{}
	c.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		switch cmd[0].Key {
		case "listCollections":
			if liveOptions == nil {
				return listReply(t), nil
			}
			return listReply(t, bson.D{
				{Key: "name", Value: "events"},
				{Key: "type", Value: kind},
				{Key: "options", Value: *liveOptions},
			}), nil
		case "listDatabases":
			return dbListReply(t, databases...), nil
		default:
			return okRaw(), nil
		}
	}
	return c
}

func collApply(t *testing.T, m *MongoModule, state string, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := m.collection().Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	return stream
}

// TestCollectionPresent_CreatesAndReportsTheDatabase — creating a collection is
// also what brings its database into being, and the outcome says whether this step
// is what did. That is the honest form of the `database.present` that does not
// exist.
func TestCollectionPresent_CreatesAndReportsTheDatabase(t *testing.T) {
	cases := []struct {
		name          string
		databases     []string
		wantDBCreated bool
	}{
		{"a database that did not exist", nil, true},
		{"a database that already held collections", []string{"appdb"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := collNode(t, nil, "", tc.databases...)
			stream := collApply(t, newModule(conn), "present", map[string]any{
				"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
			})

			if changed, _ := outcome(t, stream); !changed {
				t.Error("creating a collection must report changed=true")
			}
			if n := countCommand(conn.calls, "create"); n != 1 {
				t.Fatalf("create sent %d times, want 1", n)
			}
			if got := outputField(t, stream, "database_created"); got != tc.wantDBCreated {
				t.Errorf("output.database_created = %v, want %v", got, tc.wantDBCreated)
			}
		})
	}
}

// TestCollectionPresent_SendsOnlyDeclaredOptions — an option the operator did not
// write is not sent, so mongod's own default applies rather than this artifact's
// idea of it.
func TestCollectionPresent_SendsOnlyDeclaredOptions(t *testing.T) {
	conn := collNode(t, nil, "")
	collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 1048576,
	})

	call, ok := lastCommand(conn.calls, "create")
	if !ok {
		t.Fatal("no create was sent")
	}
	seen := map[string]bool{}
	for _, e := range call.cmd {
		seen[e.Key] = true
	}
	for _, want := range []string{"create", "capped", "size"} {
		if !seen[want] {
			t.Errorf("create did not carry %q", want)
		}
	}
	for _, unwanted := range []string{"max", "validator", "validationLevel", "collation"} {
		if seen[unwanted] {
			t.Errorf("create carried %q, which the operator did not declare", unwanted)
		}
	}
}

func TestCollectionPresent_NoOpWhenOptionsMatch(t *testing.T) {
	live := bson.D{
		{Key: "capped", Value: true},
		{Key: "size", Value: int64(1048576)},
		{Key: "validationLevel", Value: "strict"},
	}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 1048576, "validation_level": "strict",
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a collection already carrying the declared options must be a no-op, got %q", msg)
	}
	if n := countCommand(conn.calls, "collMod") + countCommand(conn.calls, "create"); n != 0 {
		t.Errorf("a no-op wrote %d time(s)", n)
	}
}

// TestCollectionPresent_MutableDriftGoesThroughCollMod — the half that IS applied.
func TestCollectionPresent_MutableDriftGoesThroughCollMod(t *testing.T) {
	live := bson.D{{Key: "validationLevel", Value: "off"}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"validation_level": "strict",
	})

	if changed, _ := outcome(t, stream); !changed {
		t.Error("a mutable drift must report changed=true")
	}
	if n := countCommand(conn.calls, "collMod"); n != 1 {
		t.Fatalf("collMod sent %d times, want 1", n)
	}
	if v, ok := commandField(conn.calls, "collMod", "validationLevel"); !ok || v != "strict" {
		t.Errorf("collMod carried validationLevel=%v, want strict", v)
	}
}

// TestCollectionPresent_ImmutableDriftIsRefused — the half that is NOT. Applying it
// means dropping the collection with its data, and that is an operator's decision.
func TestCollectionPresent_ImmutableDriftIsRefused(t *testing.T) {
	live := bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int64(1024)}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 999999,
	})

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "size") || !strings.Contains(msg, "WITH ITS DATA") {
		t.Errorf("the refusal must name the field and why it cannot be applied, got %q", msg)
	}
	if n := countCommand(conn.calls, "collMod") + countCommand(conn.calls, "drop"); n != 0 {
		t.Errorf("a refused step still wrote %d time(s) — a silent rebuild is the outcome this avoids", n)
	}
}

// TestCollectionPresent_NormalizedOptionIsNotDrift — mongod stores a declared
// `collation: { locale: "en" }` with its own defaults filled in. Comparing those
// whole would call a converged collection drifted on an IMMUTABLE option, which
// fails the step forever.
func TestCollectionPresent_NormalizedOptionIsNotDrift(t *testing.T) {
	live := bson.D{{Key: "collation", Value: bson.D{
		{Key: "locale", Value: "en"},
		{Key: "caseLevel", Value: false},
		{Key: "strength", Value: int32(3)},
		{Key: "numericOrdering", Value: false},
		{Key: "version", Value: "57.1"},
	}}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"collation": map[string]any{"locale": "en", "strength": 3},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a server-normalized option must not read as drift, got %q", msg)
	}
}

// TestCollectionPresent_NormalizedOptionStillCatchesARealChange — the subset rule
// must not become "anything matches": a declared strength the live one does not
// have is still a difference.
func TestCollectionPresent_NormalizedOptionStillCatchesARealChange(t *testing.T) {
	live := bson.D{{Key: "collation", Value: bson.D{
		{Key: "locale", Value: "en"}, {Key: "strength", Value: int32(3)},
	}}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"collation": map[string]any{"locale": "en", "strength": 2},
	})

	if msg := failureMessage(t, stream); !strings.Contains(msg, "collation") {
		t.Errorf("a real collation change must still be caught, got %q", msg)
	}
}

// TestCollectionPresent_AbsentLiveOptionMatchingADefaultIsNotDrift — mongod omits
// an option left at its default rather than storing it, so `capped: false` against
// a stored nothing is not a change.
func TestCollectionPresent_AbsentLiveOptionMatchingADefaultIsNotDrift(t *testing.T) {
	live := bson.D{}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events", "capped": false,
	})
	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a declared default against a stored nothing must be a no-op, got %q", msg)
	}
}

// TestCollectionPresent_ViewIsRefused — a view answering to the name is a different
// subject, and collMod on one does something else.
func TestCollectionPresent_ViewIsRefused(t *testing.T) {
	live := bson.D{{Key: "viewOn", Value: "raw"}}
	conn := collNode(t, &live, "view", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
	})
	if msg := failureMessage(t, stream); !strings.Contains(msg, "view") {
		t.Errorf("expected a refusal naming the kind, got %q", msg)
	}
}

func TestCollectionAbsent_DropsAndIsIdempotent(t *testing.T) {
	t.Run("drops an existing collection", func(t *testing.T) {
		live := bson.D{}
		conn := collNode(t, &live, "collection", "appdb")
		stream := collApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		})
		if changed, _ := outcome(t, stream); !changed {
			t.Error("dropping an existing collection must report changed=true")
		}
		if n := countCommand(conn.calls, "drop"); n != 1 {
			t.Errorf("drop sent %d times, want 1", n)
		}
	})

	t.Run("a collection that is not there is a no-op", func(t *testing.T) {
		conn := collNode(t, nil, "")
		stream := collApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		})
		if changed, msg := outcome(t, stream); changed {
			t.Errorf("expected a no-op, got %q", msg)
		}
		if n := countCommand(conn.calls, "drop"); n != 0 {
			t.Errorf("a no-op sent drop %d times", n)
		}
	})
}

// TestValidateCollection_RequiresTheDatabase — `database` does NOT default to admin
// the way the `user` object's does: a collection in the admin database is almost
// never intended, and a default there would create one silently.
func TestValidateCollection_RequiresTheDatabase(t *testing.T) {
	errs := validateCollectionPresent(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "events",
	}))
	if !strings.Contains(strings.Join(errs, "; "), "params.database") {
		t.Errorf("expected the database to be required, got %v", errs)
	}
}

func TestValidateCollection_RefusesValuesOutsideTheClosedSets(t *testing.T) {
	cases := []struct{ key, value, wantErr string }{
		{"validation_level", "sometimes", "off|strict|moderate"},
		{"validation_action", "shout", "error|warn"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			errs := validateCollectionPresent(mustFields(t, map[string]any{
				"addr": "127.0.0.1:27017", "database": "appdb", "name": "events", tc.key: tc.value,
			}))
			if !strings.Contains(strings.Join(errs, "; "), tc.wantErr) {
				t.Errorf("expected a refusal naming the closed set, got %v", errs)
			}
		})
	}
}

// TestCollectionPresent_CappedSizeIsRoundedTheWayMongodStoresIt — mongod rounds a
// capped collection's size UP to a multiple of 256, so `size: 1000000` comes back
// as 1000192. Compared raw that is a difference on an IMMUTABLE option, which means
// the collection this very step created fails its own second apply, permanently,
// naming a field the operator wrote correctly.
func TestCollectionPresent_CappedSizeIsRoundedTheWayMongodStoresIt(t *testing.T) {
	live := bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int64(1000192)}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 1000000,
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("the size mongod rounded up must not read as drift, got %q", msg)
	}
}

// TestCollectionPresent_TimeseriesRoundTripsAsANoOp — `timeseries` is offered as a
// create option and listCollections reports what it created as `type: "timeseries"`.
// A flat "must be an ordinary collection" check failed the SECOND apply of a step
// that had succeeded on the first, forever, on a collection this artifact made
// itself.
func TestCollectionPresent_TimeseriesRoundTripsAsANoOp(t *testing.T) {
	declared := map[string]any{"timeField": "ts", "metaField": "meta"}
	live := bson.D{{Key: "timeseries", Value: bson.D{
		{Key: "timeField", Value: "ts"},
		{Key: "metaField", Value: "meta"},
		{Key: "granularity", Value: "seconds"},            // mongod fills these in
		{Key: "bucketMaxSpanSeconds", Value: int32(3600)}, //
	}}}
	conn := collNode(t, &live, "timeseries", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "weather",
		"timeseries": declared,
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a time-series collection matching its declaration must be a no-op, got %q", msg)
	}
}

// TestCollectionPresent_KindMismatchIsStillRefused — the fix above must not have
// turned the kind check off: a plain collection declared where a time-series one
// lives, and a view either way, are still refusals.
func TestCollectionPresent_KindMismatchIsStillRefused(t *testing.T) {
	cases := []struct {
		name     string
		liveKind string
		params   map[string]any
	}{
		{"a time-series collection where a plain one was declared", "timeseries",
			map[string]any{"addr": "127.0.0.1:27017", "database": "appdb", "name": "events"}},
		{"a view where a time-series collection was declared", "view",
			map[string]any{"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
				"timeseries": map[string]any{"timeField": "ts"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := bson.D{}
			conn := collNode(t, &live, tc.liveKind, "appdb")
			stream := collApply(t, newModule(conn), "present", tc.params)
			if msg := failureMessage(t, stream); !strings.Contains(msg, tc.liveKind) {
				t.Errorf("the refusal must name the live kind, got %q", msg)
			}
			if n := countCommand(conn.calls, "collMod"); n != 0 {
				t.Errorf("a refused step still sent collMod %d times", n)
			}
		})
	}
}

// TestCollectionPresent_SmallCappedSizeIsRaisedToTheFloor — mongod's rule has TWO
// halves: a size at or below 4096 becomes exactly 4096, and anything above is
// raised to a multiple of 256. Implementing only the second half left every
// declared `size <= 3840` failing its own second apply, on an IMMUTABLE option,
// naming a field the operator wrote correctly.
func TestCollectionPresent_SmallCappedSizeIsRaisedToTheFloor(t *testing.T) {
	live := bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int64(4096)}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 1024,
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a size mongod raised to its 4096 floor must not read as drift, got %q", msg)
	}
}

// TestCollectionPresent_SimpleCollationIsStoredAsNothing — "simple" is the binary
// collator, i.e. no collation, and mongod stores no `collation` field for it. The
// absent live value would otherwise read as an immutable drift and fail the step
// forever on the collection it created itself.
func TestCollectionPresent_SimpleCollationIsStoredAsNothing(t *testing.T) {
	live := bson.D{}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"collation": map[string]any{"locale": "simple"},
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("collation simple stores nothing and must not read as drift, got %q", msg)
	}

	// A real locale against a stored nothing is still a difference.
	conn2 := collNode(t, &live, "collection", "appdb")
	stream2 := collApply(t, newModule(conn2), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"collation": map[string]any{"locale": "en"},
	})
	if msg := failureMessage(t, stream2); !strings.Contains(msg, "collation") {
		t.Errorf("a real collation against a stored nothing must still be caught, got %q", msg)
	}
}

// TestCollectionProbes_UnreadableReplyIsAFailureNotAbsence — decoding a reply this
// code cannot parse as "the object is not there" makes `absent` report a no-op
// about something still present, and `present` create a duplicate. An EMPTY batch
// is the genuine "not found"; anything unparseable is a failure.
func TestCollectionProbes_UnreadableReplyIsAFailureNotAbsence(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			conn := &fakeConn{}
			conn.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
				if cmd[0].Key == "listCollections" {
					return rawDoc(t, bson.D{{Key: "ok", Value: int32(1)}}), nil // no cursor
				}
				return okRaw(), nil
			}
			stream := collApply(t, newModule(conn), state, map[string]any{
				"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
			})
			if msg := failureMessage(t, stream); !strings.Contains(msg, "listCollections") {
				t.Errorf("an unreadable probe reply must fail, got %q", msg)
			}
			for _, write := range []string{"create", "drop", "collMod"} {
				if n := countCommand(conn.calls, write); n != 0 {
					t.Errorf("%s was sent %d times after an unreadable probe", write, n)
				}
			}
		})
	}
}

// TestCollectionPresent_UnreadableDatabaseListIsAFailure — `database_created` must
// not be able to claim this step brought a database into being that was already
// there, so an unreadable listDatabases is an error rather than "absent".
func TestCollectionPresent_UnreadableDatabaseListIsAFailure(t *testing.T) {
	conn := &fakeConn{}
	conn.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		switch cmd[0].Key {
		case "listCollections":
			return listReply(t), nil // the collection is absent, so the DB is read
		case "listDatabases":
			return rawDoc(t, bson.D{{Key: "ok", Value: int32(1)}}), nil // no databases array
		}
		return okRaw(), nil
	}
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
	})

	if msg := failureMessage(t, stream); !strings.Contains(msg, "listDatabases") {
		t.Errorf("an unreadable listDatabases must fail, got %q", msg)
	}
	if n := countCommand(conn.calls, "create"); n != 0 {
		t.Errorf("create was sent %d times after an unreadable listDatabases", n)
	}
}

// TestCollectionPresent_ZeroMaxIsStoredAsNothing — mongod omits `max: 0` from a
// capped collection's options, and `max` is IMMUTABLE, so an absent live value
// against a declared 0 would fail the step forever on the collection it created.
//
// This is the case that narrowing the "defaulted away" rule to booleans — needed so
// `expire_after_seconds: 0` stays a real declaration on an index — took with it. The
// rule is per OPTION now, which is what it always was a fact about.
func TestCollectionPresent_ZeroMaxIsStoredAsNothing(t *testing.T) {
	live := bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int64(4096)}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 4096, "max": 0,
	})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a declared max of 0, which mongod stores as nothing, must not read as drift, got %q", msg)
	}
}

// TestCollectionPresent_RealMaxAgainstAStoredNothingIsStillDrift — the exemption is
// about ZERO, not about `max`: a real cap the live collection does not carry is a
// difference, and an immutable one.
func TestCollectionPresent_RealMaxAgainstAStoredNothingIsStillDrift(t *testing.T) {
	live := bson.D{{Key: "capped", Value: true}, {Key: "size", Value: int64(4096)}}
	conn := collNode(t, &live, "collection", "appdb")
	stream := collApply(t, newModule(conn), "present", map[string]any{
		"addr": "127.0.0.1:27017", "database": "appdb", "name": "events",
		"capped": true, "size": 4096, "max": 500,
	})

	if msg := failureMessage(t, stream); !strings.Contains(msg, "max") {
		t.Errorf("a real document cap the collection does not have must still be caught, got %q", msg)
	}
}
