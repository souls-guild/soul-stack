// The `index` object — one index on one collection, through createIndexes /
// dropIndexes / collMod.
//
// ★ THE KEY IS A LIST, NOT A MAP, AND THAT IS NOT A STYLE CHOICE
//
// An index key is an ORDERED document: an index on { user_id: 1, created_at: -1 }
// is a different index from one on { created_at: -1, user_id: 1 }, with different
// query plans. A YAML map reaches this plugin as a `structpb.Struct`, which is a Go
// map, and its iteration order is not defined — the same limitation `command.run`
// documents when it says only single-field commands are reliable. Declaring the key
// as a map would therefore build a DIFFERENT INDEX from one run to the next.
//
// So `keys` is a list of { field, order }, order preserved, and it is compared with
// the live key through [orderedKeyString] rather than through [canonicalValue],
// which sorts map keys and would call the two indexes above equal.
//
// ★ AN INDEX CANNOT BE MODIFIED
//
// Changing what an index is ON — its key, its uniqueness, its partial filter, its
// collation — is not an operation mongod offers. It means dropping the index and
// building a new one, which on a large collection is a long, IO-heavy rebuild
// during which the queries that relied on it have no index. So a declared key or
// immutable option that differs from the live one is a FAILURE naming the field,
// and the operator drops it deliberately (`index.absent`) if that is what they
// meant. Only `expire_after_seconds` and `hidden` can be changed in place, through
// collMod, and those two this action does change.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/structpb"
)

// idIndexName is the index mongod creates and maintains itself. It cannot be
// created and cannot be dropped, so it is refused as a subject on both actions —
// in [validateIndexSubject], which [object.Apply] runs as well as Validate, so a
// runner that skips the Validate RPC meets the same refusal.
const idIndexName = "_id_"

// indexOption is one declared option of an index, paired with the field mongod
// stores it under and with whether collMod can change it in place.
type indexOption struct {
	param   string
	field   string
	mutable bool
	subset  bool
}

// indexOptions is the closed set this object models, in a fixed order so a built
// document is byte-stable and a multi-field report is deterministic.
var indexOptions = []indexOption{
	// MUTABLE — collMod takes these on a live index. `expire_after_seconds` only
	// when the index ALREADY has one; see [diffIndexOptions].
	{param: "expire_after_seconds", field: "expireAfterSeconds", mutable: true},
	{param: "hidden", field: "hidden", mutable: true},

	// IMMUTABLE — changing one means dropping the index and rebuilding it.
	{param: "collation", field: "collation", subset: true},
	{param: "partial_filter_expression", field: "partialFilterExpression"},
	{param: "sparse", field: "sparse"},
	{param: "unique", field: "unique"},

	// `wildcard_projection` is deliberately NOT modelled. mongod rewrites the
	// projection it stores — dotted paths expanded into nested documents, 1/0 turned
	// into true/false — so a declared `{"payload.secret": 0}` reads back as
	// `{payload: {secret: false}}` and can never compare equal. On an immutable
	// option that is a step failing forever on an index it created itself. A
	// wildcard index without a projection (`$**`) is served here; one with a
	// projection belongs to mongo.command.run until the comparison is written
	// against a live server rather than guessed at.
}

// indexKeyPart is one field of the key, in the position the operator wrote it.
type indexKeyPart struct {
	field string
	order any // int64 (1 or -1) or a string index type ("text", "2dsphere", "hashed", …)
}

// parseIndexKeys reads params.keys, preserving order.
//
// `order` accepts an integer or a string and REFUSES anything else, rather than
// defaulting: an ascending index where a descending one was meant is a silently
// wrong index, and a wrong index is a query plan nobody notices until it is slow.
func parseIndexKeys(v *structpb.Value) ([]indexKeyPart, error) {
	items := listField(v)
	if len(items) == 0 {
		return nil, fmt.Errorf("params.keys: must be a non-empty list of { field, order } — a LIST, because an index key is ORDERED and a map's order is not preserved")
	}
	out := make([]indexKeyPart, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, it := range items {
		addr := fmt.Sprintf("params.keys[%d]", i)
		spec := structField(it)
		if spec == nil {
			return nil, fmt.Errorf("%s: must be an object { field, order }", addr)
		}
		field, err := stringField(spec, "field", addr+".field", "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(field) == "" {
			return nil, fmt.Errorf("%s.field: must be a non-empty document field path", addr)
		}
		if seen[field] {
			return nil, fmt.Errorf("%s.field: %q appears twice in the key — a field indexes once", addr, field)
		}
		seen[field] = true

		order, err := parseIndexOrder(spec["order"], addr+".order")
		if err != nil {
			return nil, err
		}
		out = append(out, indexKeyPart{field: field, order: order})
	}
	return out, nil
}

// parseIndexOrder reads one key value: a direction or an index type.
func parseIndexOrder(v *structpb.Value, addr string) (any, error) {
	if v == nil || isNull(v) {
		return int64(1), nil
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		if k.NumberValue != float64(int64(k.NumberValue)) {
			return nil, fmt.Errorf("%s: must be 1 or -1 (or an index type as a string), got %v", addr, k.NumberValue)
		}
		n := int64(k.NumberValue)
		if n != 1 && n != -1 {
			return nil, fmt.Errorf("%s: a numeric direction must be 1 (ascending) or -1 (descending), got %d", addr, n)
		}
		return n, nil
	case *structpb.Value_StringValue:
		if strings.TrimSpace(k.StringValue) == "" {
			return nil, fmt.Errorf("%s: an index type must be a non-empty string (2dsphere, 2d, hashed, …)", addr)
		}
		// ★ A TEXT INDEX IS NOT STORED UNDER THE KEY IT IS CREATED WITH. mongod
		// rewrites it: `key` becomes {_fts: "text", _ftsx: 1} and the fields move
		// into `weights`. So a declared {title: "text"} could never match what
		// listIndexes reads back, and every apply after the first would refuse the
		// index as "a different key" and tell the operator to drop the one this
		// artifact had just built. Refusing it here is the honest answer: a promise
		// that cannot be kept is worse than an absent feature, and mongo.command.run
		// with createIndexes serves a text index in the meantime.
		if k.StringValue == "text" {
			return nil, fmt.Errorf(
				"%s: this object does not manage TEXT indexes — mongod stores one under a rewritten key "+
					"({_fts, _ftsx}, the fields moved to `weights`), so it could never be recognised as converged "+
					"and every re-apply would ask you to drop it. Create it with mongo.command.run { createIndexes: … }",
				addr)
		}
		return k.StringValue, nil
	default:
		return nil, fmt.Errorf("%s: must be 1, -1, or an index type as a string, got %s", addr, valueTypeName(v))
	}
}

// keyDoc renders the declared key in the order it was written — the document that
// goes into createIndexes.
func keyDoc(parts []indexKeyPart) bson.D {
	doc := make(bson.D, 0, len(parts))
	for _, p := range parts {
		doc = append(doc, bson.E{Key: p.field, Value: p.order})
	}
	return doc
}

// keyString is the comparable form of a declared key.
func keyString(parts []indexKeyPart) string {
	pairs := make([][2]any, 0, len(parts))
	for _, p := range parts {
		pairs = append(pairs, [2]any{p.field, p.order})
	}
	return orderedKeyString(pairs)
}

// liveKeyString is the comparable form of the key mongod returned, read as an
// ORDERED document so the comparison stays order-sensitive.
func liveKeyString(doc bson.Raw) string {
	v, err := doc.LookupErr("key")
	if err != nil {
		return ""
	}
	sub, ok := v.DocumentOK()
	if !ok {
		return ""
	}
	var d bson.D
	if err := bson.Unmarshal(sub, &d); err != nil {
		return ""
	}
	pairs := make([][2]any, 0, len(d))
	for _, e := range d {
		pairs = append(pairs, [2]any{e.Key, e.Value})
	}
	return orderedKeyString(pairs)
}

// declaredIndexOptions reads the index options the operator actually wrote. One
// that is NOT written is not compared and not sent.
func declaredIndexOptions(f map[string]*structpb.Value) map[string]any {
	out := make(map[string]any, len(indexOptions))
	for _, opt := range indexOptions {
		v, ok := f[opt.param]
		if !ok || v == nil || isNull(v) {
			continue
		}
		out[opt.param] = valueToNative(v)
	}
	return out
}

// --- reading the live index ---

// liveIndex is what listIndexes says about one index.
type liveIndex struct {
	exists bool
	doc    bson.Raw
}

// readIndex asks listIndexes for the collection and picks the one by name.
//
// A collection whose indexes do not fit one cursor batch would need a getMore,
// which the connection interface of this artifact does not expose. The batch is 101
// documents and mongod caps a collection at 64 indexes, so the batch always holds
// all of them — the limit is the server's, not an assumption made here.
//
// A collection that does not exist answers NamespaceNotFound; that is "no such
// index", not a failure, and it is the idempotency probe for `absent`.
func readIndex(ctx context.Context, conn mongoConn, db, coll, name string) (liveIndex, error) {
	raw, err := conn.RunCommand(ctx, db, bson.D{{Key: "listIndexes", Value: coll}})
	if err != nil {
		if isMongoCode(err, codeNamespaceNotFound, "NamespaceNotFound") {
			return liveIndex{}, nil
		}
		return liveIndex{}, fmt.Errorf("listIndexes: %w", err)
	}
	docs, err := firstBatchDocs(raw)
	if err != nil {
		return liveIndex{}, fmt.Errorf("listIndexes: %w", err)
	}
	for _, d := range docs {
		v, err := d.LookupErr("name")
		if err != nil {
			continue
		}
		if s, ok := v.StringValueOK(); ok && s == name {
			return liveIndex{exists: true, doc: d}, nil
		}
	}
	return liveIndex{}, nil
}

// --- Apply ---

// applyIndexPresent creates the index, changes what can be changed in place, or
// refuses a change that would mean a rebuild.
func (m *MongoModule) applyIndexPresent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	db := stringOrEmpty(f["database"])
	coll := stringOrEmpty(f["collection"])
	name := stringOrEmpty(f["name"])

	parts, err := parseIndexKeys(f["keys"])
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	want := declaredIndexOptions(f)

	live, err := readIndex(ctx, conn, db, coll, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	if !live.exists {
		spec := bson.D{{Key: "key", Value: keyDoc(parts)}, {Key: "name", Value: name}}
		for _, opt := range indexOptions {
			if v, ok := want[opt.param]; ok {
				spec = append(spec, bson.E{Key: opt.field, Value: v})
			}
		}
		cmd := bson.D{{Key: "createIndexes", Value: coll}, {Key: "indexes", Value: bson.A{spec}}}
		if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
			return sendFailure(stream, "createIndexes: "+redactError(err, secrets...))
		}
		return sendOutcome(stream, true, fmt.Sprintf("index %q created on %s.%s", name, db, coll),
			indexOutput(db, coll, name, true))
	}

	// The key first, because a different key is a different index wearing the same
	// name, and reporting an option drift on it would be answering the wrong
	// question.
	if got, wantKey := liveKeyString(live.doc), keyString(parts); got != wantKey {
		return sendFailure(stream, fmt.Sprintf(
			"index %q on %s.%s exists on a different key: live %s, declared %s. An index CANNOT be modified — "+
				"applying this means dropping it and rebuilding, which leaves the queries that use it unindexed "+
				"for the length of the build. Drop it deliberately with mongo.index.absent if that is intended",
			name, db, coll, got, wantKey))
	}

	immutable, mutable := diffIndexOptions(live.doc, want)
	if len(immutable) > 0 {
		sort.Strings(immutable)
		return sendFailure(stream, fmt.Sprintf(
			"index %q on %s.%s exists with different %s: %s cannot be changed on a live index — "+
				"applying this means dropping it and rebuilding. Drop it deliberately with mongo.index.absent "+
				"if that is intended",
			name, db, coll, plural("option", len(immutable)), strings.Join(immutable, ", ")))
	}
	if len(mutable) == 0 {
		return sendOutcome(stream, false, fmt.Sprintf("index %q on %s.%s already present as declared (no-op)", name, db, coll),
			indexOutput(db, coll, name, true))
	}

	sort.Strings(mutable)
	spec := bson.D{{Key: "name", Value: name}}
	for _, opt := range indexOptions {
		if !opt.mutable {
			continue
		}
		if v, ok := want[opt.param]; ok {
			spec = append(spec, bson.E{Key: opt.field, Value: v})
		}
	}
	cmd := bson.D{{Key: "collMod", Value: coll}, {Key: "index", Value: spec}}
	if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
		return sendFailure(stream, "collMod: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true,
		fmt.Sprintf("index %q on %s.%s updated: %s", name, db, coll, strings.Join(mutable, ", ")),
		indexOutput(db, coll, name, true))
}

// diffIndexOptions splits what differs into what would need a rebuild and what
// collMod can do in place. Only DECLARED options are compared.
//
// ★ A TTL CANNOT BE ADDED OR REMOVED IN PLACE. `collMod {index: {name,
// expireAfterSeconds}}` changes an existing TTL; against an index that has none it
// answers InvalidOptions — "no expireAfterSeconds field to update" — in the middle
// of a run, and re-running does not help. So `expire_after_seconds` counts as
// mutable only when the live index ALREADY carries one, and otherwise falls to the
// immutable side, which is a refusal naming the field before anything is sent.
func diffIndexOptions(live bson.Raw, want map[string]any) (immutable, mutable []string) {
	for _, opt := range indexOptions {
		w, declared := want[opt.param]
		if !declared {
			continue
		}
		got, present := lookupNative(live, opt.field)
		match := present && (opt.subset && matchesDeclared(got, w) || !opt.subset && sameValue(got, w))
		if match {
			continue
		}
		// mongod omits an option left at its default rather than storing it, so an
		// absent live value against a declared `false` is not a difference — nor is
		// `collation: { locale: "simple" }`, which is "no collation" and is stored
		// as nothing. Only those: a declared `expire_after_seconds: 0` MEANS
		// something.
		// zeroOmitted is deliberately NOT set for any index option: a declared
		// `expire_after_seconds: 0` means "expire at the indexed date itself", so an
		// index with no TTL really does differ from it.
		if !present && omittedByServer(collectionOption{param: opt.param}, w) {
			continue
		}
		if opt.mutable && !(opt.param == "expire_after_seconds" && !present) {
			mutable = append(mutable, opt.param)
		} else {
			immutable = append(immutable, opt.param)
		}
	}
	return immutable, mutable
}

// applyIndexAbsent drops the index. Idempotent through the same listIndexes probe:
// an index that is not there — on a collection that may not be there either — is a
// no-op.
func (m *MongoModule) applyIndexAbsent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	db := stringOrEmpty(f["database"])
	coll := stringOrEmpty(f["collection"])
	name := stringOrEmpty(f["name"])

	live, err := readIndex(ctx, conn, db, coll, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if !live.exists {
		return sendOutcome(stream, false, fmt.Sprintf("index %q already absent on %s.%s", name, db, coll),
			indexOutput(db, coll, name, false))
	}
	cmd := bson.D{{Key: "dropIndexes", Value: coll}, {Key: "index", Value: name}}
	if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
		return sendFailure(stream, "dropIndexes: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("index %q dropped on %s.%s", name, db, coll),
		indexOutput(db, coll, name, false))
}

func indexOutput(db, coll, name string, present bool) map[string]any {
	return map[string]any{"database": db, "collection": coll, "name": name, "present": present}
}

// --- Validate ---

// validateIndexPresent refuses what Apply would: the subject, `_id_` as a name, and
// every malformed key — [parseIndexKeys] is the same function Apply calls, so the
// two cannot disagree about what a well-formed key is (NIM-786).
func validateIndexPresent(f map[string]*structpb.Value) []string {
	errs := validateIndexSubject(f)
	if _, err := parseIndexKeys(f["keys"]); err != nil {
		errs = append(errs, err.Error())
	}
	if v, ok := f["expire_after_seconds"]; ok && v != nil && !isNull(v) && intOrDefault(v, 0) < 0 {
		errs = append(errs, "params.expire_after_seconds: must be >= 0 (seconds a document survives after its indexed date)")
	}
	return errs
}

// validateIndexAbsent — the subject alone. The key does not matter to a drop, which
// addresses an index by name.
func validateIndexAbsent(f map[string]*structpb.Value) []string {
	return validateIndexSubject(f)
}

// validateIndexSubject is what both actions require. `_id_` is refused on both:
// mongod creates and maintains it itself and refuses to drop it, so a step naming
// it can only fail — better here, before the run, than as a server error inside it.
func validateIndexSubject(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "database")...)
	errs = append(errs, requireString(f, "collection")...)
	errs = append(errs, requireString(f, "name")...)

	if stringOrEmpty(f["name"]) == idIndexName {
		errs = append(errs, fmt.Sprintf(
			"params.name: %q is the index mongod creates and maintains itself — it can be neither created nor dropped", idIndexName))
	}
	return errs
}
