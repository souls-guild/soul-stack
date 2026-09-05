// The `collection` object — one MongoDB collection, created with its options and
// dropped, through the `create` / `drop` / `collMod` commands.
//
// ★ THE CONTENT OF THIS OBJECT IS THE MUTABLE/IMMUTABLE SPLIT
//
// Some collection options can be changed on a live collection (`collMod` takes
// them) and some cannot, because changing them means rebuilding the collection and
// therefore moving the data. This object does the first and REFUSES the second by
// name:
//
//	mutable    validator, validation_level, validation_action  -> collMod
//	immutable  capped, size, max, collation, timeseries, clustered_index
//
// A declared immutable option that differs from the live one is a failure that says
// which field and why, not a silent no-op and not a drop-and-recreate. The only way
// to "fix" it is to lose the collection's data, and that is an operator's decision,
// not a converge step's — the same reason `redis.cluster.node-removed` migrates
// slots before evicting rather than deciding for anyone.
//
// ★ A DATABASE IS NOT A SEPARATE SUBJECT
//
// MongoDB has no command that creates a database: one exists once it holds a
// collection. So creating a collection here is also what brings its database into
// being, and the outcome reports `database_created` — rather than there being a
// `database.present` action whose whole effect would be nothing observable. That is
// why the `database` object of this artifact serves `absent` alone.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/structpb"
)

// collectionOption is one declared option of a collection, paired with the field
// mongod stores it under and with whether collMod can change it on a live
// collection. One table, so the create path, the diff and the refusal cannot drift.
type collectionOption struct {
	param   string
	field   string
	mutable bool

	// zeroOmitted says the server stores NOTHING for a declared zero, so an absent
	// live value against `0` is not a difference. It is per-option and not a rule
	// about numbers: mongod omits `max: 0` from a capped collection's options, but a
	// declared `expire_after_seconds: 0` on an index MEANS "expire at the indexed
	// date" and an absent live value there is a real difference.
	zeroOmitted bool

	// subset says the server NORMALIZES this option by filling in its own
	// defaults, so only the keys the operator wrote are compared
	// ([matchesDeclared]). Without it a declared `collation: { locale: "en" }`
	// would differ from the eight-key document mongod stores — and differ on an
	// IMMUTABLE option, where this artifact's answer is to fail the step.
	subset bool
}

// collectionOptions is the closed set this object models, in a fixed order so a
// built document is byte-stable and a multi-field report is deterministic.
//
// `expireAfterSeconds` is deliberately NOT here. On an ordinary collection a TTL
// lives on an INDEX, not on the collection, and declaring it at this level would
// promise an operator a knob that only does something on the time-series and
// clustered forms — the `index` object is where a TTL belongs.
var collectionOptions = []collectionOption{
	{param: "capped", field: "capped"},
	{param: "clustered_index", field: "clusteredIndex", subset: true},
	{param: "collation", field: "collation", subset: true},
	{param: "max", field: "max", zeroOmitted: true},
	{param: "size", field: "size", zeroOmitted: true},
	{param: "timeseries", field: "timeseries", subset: true},
	{param: "validation_action", field: "validationAction", mutable: true},
	{param: "validation_level", field: "validationLevel", mutable: true},
	// EXACT, not subset: a validator that lost a clause out of band has really
	// changed, and a subset comparison would call that converged.
	{param: "validator", field: "validator", mutable: true},
}

// validationLevels / validationActions are the closed sets mongod accepts. They are
// checked in Validate because they are visible there, and a typo in one otherwise
// surfaces as a command error in the middle of a run (NIM-786).
var validationLevels = map[string]bool{"off": true, "strict": true, "moderate": true}
var validationActions = map[string]bool{"error": true, "warn": true}

// declaredOptions reads the collection options the operator actually wrote, in
// native form. An option that is NOT written is not compared and not sent: mongod's
// own default applies on create, and the live value survives on a collMod.
func declaredOptions(f map[string]*structpb.Value) (map[string]any, error) {
	out := make(map[string]any, len(collectionOptions))
	for _, opt := range collectionOptions {
		v, ok := f[opt.param]
		if !ok || v == nil || isNull(v) {
			continue
		}
		out[opt.param] = valueToNative(v)
	}

	// The two closed-set options are checked here rather than only in Validate,
	// because Apply is the path a runner is guaranteed to take.
	if lvl, ok := out["validation_level"].(string); ok && !validationLevels[lvl] {
		return nil, fmt.Errorf("params.validation_level: %q is not one of off|strict|moderate", lvl)
	}
	if act, ok := out["validation_action"].(string); ok && !validationActions[act] {
		return nil, fmt.Errorf("params.validation_action: %q is not one of error|warn", act)
	}
	return out, nil
}

// liveCollection is what listCollections says: whether it is there, and the options
// it was created with.
type liveCollection struct {
	exists  bool
	kind    string // "collection", "view", "timeseries"
	options bson.Raw
}

// readCollection asks listCollections for ONE collection by name. Absent comes back
// as an empty batch rather than an error — the idempotency probe, same shape as
// [userExists] and [readRole].
func readCollection(ctx context.Context, conn mongoConn, db, name string) (liveCollection, error) {
	raw, err := conn.RunCommand(ctx, db, bson.D{
		{Key: "listCollections", Value: 1},
		{Key: "filter", Value: bson.D{{Key: "name", Value: name}}},
	})
	if err != nil {
		return liveCollection{}, fmt.Errorf("listCollections: %w", err)
	}
	doc, ok, err := firstBatchDoc(raw)
	if err != nil {
		return liveCollection{}, fmt.Errorf("listCollections: %w", err)
	}
	if !ok {
		return liveCollection{}, nil
	}
	out := liveCollection{exists: true}
	if v, err := doc.LookupErr("type"); err == nil {
		out.kind, _ = v.StringValueOK()
	}
	if v, err := doc.LookupErr("options"); err == nil {
		if sub, ok := v.DocumentOK(); ok {
			out.options = sub
		}
	}
	return out, nil
}

// firstBatchDoc lifts the first document out of a `cursor.firstBatch` reply, which
// is how listCollections and listIndexes both answer.
func firstBatchDoc(raw bson.Raw) (bson.Raw, bool, error) {
	docs, err := firstBatchDocs(raw)
	if err != nil || len(docs) == 0 {
		return nil, false, err
	}
	return docs[0], true, nil
}

// firstBatchDocs lifts the whole first batch.
//
// A cursor with more than one batch is not read further, and for these two callers
// that is correct rather than a shortcut: listCollections is filtered to one name,
// and a collection with more indexes than one batch holds would need a getMore this
// artifact's connection interface does not expose. The second case is noted where it
// could bite ([readIndex]).
func firstBatchDocs(raw bson.Raw) ([]bson.Raw, error) {
	// A reply this cannot READ is an error, not an empty batch. Decoding it as
	// "the object is not there" makes `collection.absent` / `index.absent` report a
	// no-op about something that is still there, and `present` create a duplicate.
	cur, err := raw.LookupErr("cursor")
	if err != nil {
		return nil, fmt.Errorf("reply carries no cursor")
	}
	curDoc, ok := cur.DocumentOK()
	if !ok {
		return nil, fmt.Errorf("cursor is not a document")
	}
	batch, err := curDoc.LookupErr("firstBatch")
	if err != nil {
		return nil, fmt.Errorf("cursor carries no firstBatch")
	}
	arr, ok := batch.ArrayOK()
	if !ok {
		return nil, fmt.Errorf("firstBatch is not an array")
	}
	vals, err := arr.Values()
	if err != nil {
		return nil, fmt.Errorf("unreadable firstBatch: %w", err)
	}
	out := make([]bson.Raw, 0, len(vals))
	for _, v := range vals {
		d, ok := v.DocumentOK()
		if !ok {
			return nil, fmt.Errorf("a firstBatch entry is not a document")
		}
		out = append(out, d)
	}
	return out, nil
}

// databaseExists reports whether the database is in listDatabases. It is read
// BEFORE a create so the outcome can say honestly whether this step is what brought
// the database into being.
func databaseExists(ctx context.Context, conn mongoConn, name string) (bool, error) {
	raw, err := conn.RunCommand(ctx, adminDB, bson.D{
		{Key: "listDatabases", Value: 1},
		{Key: "nameOnly", Value: true},
		{Key: "filter", Value: bson.D{{Key: "name", Value: name}}},
	})
	if err != nil {
		return false, fmt.Errorf("listDatabases: %w", err)
	}
	// A reply this cannot read is an ERROR, not "the database is absent". The
	// difference reaches an operator: `database_created` would otherwise claim this
	// step brought a database into being that had been there all along.
	val, lookupErr := raw.LookupErr("databases")
	if lookupErr != nil {
		return false, fmt.Errorf("listDatabases: reply carries no databases array")
	}
	arr, ok := val.ArrayOK()
	if !ok {
		return false, fmt.Errorf("listDatabases: databases is not an array")
	}
	vals, err := arr.Values()
	if err != nil {
		return false, fmt.Errorf("listDatabases: unreadable databases array: %w", err)
	}
	return len(vals) > 0, nil
}

// --- Apply ---

// applyCollectionPresent creates the collection or brings its MUTABLE options to
// what was declared. An immutable option that drifted is refused by name.
func (m *MongoModule) applyCollectionPresent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	db := stringOrEmpty(f["database"])
	name := stringOrEmpty(f["name"])

	want, err := declaredOptions(f)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	live, err := readCollection(ctx, conn, db, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	if !live.exists {
		dbExisted, err := databaseExists(ctx, conn, db)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		cmd := bson.D{{Key: "create", Value: name}}
		for _, opt := range collectionOptions {
			if v, ok := want[opt.param]; ok {
				cmd = append(cmd, bson.E{Key: opt.field, Value: v})
			}
		}
		if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
			return sendFailure(stream, "create: "+redactError(err, secrets...))
		}
		return sendOutcome(stream, true, fmt.Sprintf("collection %s.%s created", db, name), map[string]any{
			"database":         db,
			"name":             name,
			"present":          true,
			"database_created": !dbExisted,
		})
	}

	// The live KIND must be the one that was declared. `timeseries` is a create
	// option here, and listCollections reports what it created as
	// `type: "timeseries"` — so a flat "must be an ordinary collection" check would
	// fail the second apply of a step that succeeded on the first, permanently, on a
	// collection this artifact made itself. What is actually wrong is a MISMATCH: a
	// view, or a time-series collection where a plain one was declared.
	if wantKind := declaredKind(want); live.kind != "" && live.kind != wantKind {
		return sendFailure(stream, fmt.Sprintf(
			"%s.%s exists as a %q and params declare a %q: a collection cannot be converted between the two, "+
				"and this action does not manage a view", db, name, live.kind, wantKind))
	}

	immutable, mutable := diffCollectionOptions(live.options, want)
	if len(immutable) > 0 {
		sort.Strings(immutable)
		return sendFailure(stream, fmt.Sprintf(
			"collection %s.%s exists with different %s: %s cannot be changed on a live collection BY THIS ACTION — "+
				"applying it means dropping the collection WITH ITS DATA, which this step will not do. "+
				"(A capped collection can be resized in place from MongoDB 6.0 with "+
				"mongo.command.run { collMod: %q, cappedSize: … }; this action does not, because the operation is "+
				"server-version dependent and silently doing nothing on an older one would be worse.)",
			db, name, plural("option", len(immutable)), strings.Join(immutable, ", "), name))
	}
	if len(mutable) == 0 {
		return sendOutcome(stream, false, fmt.Sprintf("collection %s.%s already present with the declared options (no-op)", db, name),
			map[string]any{"database": db, "name": name, "present": true, "database_created": false})
	}

	sort.Strings(mutable)
	cmd := bson.D{{Key: "collMod", Value: name}}
	for _, opt := range collectionOptions {
		if !opt.mutable {
			continue
		}
		if v, ok := want[opt.param]; ok {
			cmd = append(cmd, bson.E{Key: opt.field, Value: v})
		}
	}
	if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
		return sendFailure(stream, "collMod: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true,
		fmt.Sprintf("collection %s.%s updated: %s", db, name, strings.Join(mutable, ", ")),
		map[string]any{"database": db, "name": name, "present": true, "database_created": false})
}

// diffCollectionOptions splits what differs into what cannot be changed and what
// can. Only DECLARED options are compared: one the operator did not write is one
// this step has no opinion about, so leaving it alone is the whole of the rule.
func diffCollectionOptions(liveOpts bson.Raw, want map[string]any) (immutable, mutable []string) {
	for _, opt := range collectionOptions {
		w, declared := want[opt.param]
		if !declared {
			continue
		}
		live, present := lookupNative(liveOpts, opt.field)
		if present && optionMatches(opt, storedForm(opt, w), live) {
			continue
		}
		// An absent live option matching a declared default is not a difference:
		// mongod omits `capped: false`, and stores NO collation at all for the
		// simple binary collator.
		if !present && omittedByServer(opt, w) {
			continue
		}
		if opt.mutable {
			mutable = append(mutable, opt.param)
		} else {
			immutable = append(immutable, opt.param)
		}
	}
	return immutable, mutable
}

// optionMatches applies the comparison this option asks for.
func optionMatches(opt collectionOption, want, live any) bool {
	if opt.subset {
		return matchesDeclared(live, want)
	}
	return sameValue(live, want)
}

// storedForm is the declared value AS MONGOD WILL HAVE STORED IT, for the options
// the server rewrites on the way in.
//
// `size` is the one that bites: mongod rounds a capped collection's size UP to a
// multiple of 256, so `size: 1000000` comes back as 1000192. Compared raw that is a
// difference on an IMMUTABLE option, which means a collection created by this very
// step fails its own second apply, permanently, naming a field the operator wrote
// correctly. Rounding the declared value the same way is the comparison actually
// being asked for: "is the live collection what this declaration produces".
// mongod's two rules for a capped collection's size, which apply in this order: a
// size at or below the floor becomes exactly the floor, and anything above it is
// raised to a multiple of the alignment. Implementing only the second half left
// every declared `size <= 3840` failing its own second apply.
const (
	cappedSizeFloor     = 4096
	cappedSizeAlignment = 256
)

func storedForm(opt collectionOption, want any) any {
	if opt.param != "size" {
		return want
	}
	n, ok := want.(int64)
	if !ok || n <= 0 {
		return want
	}
	if n <= cappedSizeFloor {
		return int64(cappedSizeFloor)
	}
	return (n + cappedSizeAlignment - 1) / cappedSizeAlignment * cappedSizeAlignment
}

// declaredKind is the collection kind the params describe, in the vocabulary
// listCollections answers in.
func declaredKind(want map[string]any) string {
	if _, ok := want["timeseries"]; ok {
		return "timeseries"
	}
	return "collection"
}

// isDefaultedAway reports whether a declared value is one mongod stores by OMITTING
// the field, so an absent live value is not a difference.
//
// ★ BOOLEANS ONLY, and the restriction is the whole point. It used to accept a zero
// NUMBER too, and that swallowed a real declaration: `expire_after_seconds: 0` is a
// documented, meaningful value — expire at the indexed date itself — so on a plain
// index with no TTL the difference would be skipped and the step would report
// `changed=false, already present as declared` about an index that is not a TTL
// index at all. An idempotency lie, from a rule meant to prevent a cosmetic one.
func isDefaultedAway(v any) bool {
	b, ok := v.(bool)
	return (ok && !b) || v == nil
}

// omittedByServer answers, for ONE option, whether an absent live value matches
// the declared one because mongod stores nothing for it. Three cases, and each is
// a fact about the server rather than a rule about the type: a false boolean, a
// zero on an option whose zero is omitted, and the simple collator.
func omittedByServer(opt collectionOption, want any) bool {
	if isDefaultedAway(want) || isSimpleCollation(opt, want) {
		return true
	}
	if !opt.zeroOmitted {
		return false
	}
	return isNumeric(want) && asFloat(want) == 0
}

// isSimpleCollation reports a declared `collation: { locale: "simple" }`, which
// mongod does NOT store: "simple" is the binary collator, i.e. no collation at all,
// so the created object comes back with no `collation` field. Without this the
// absent live value reads as an immutable drift and the step fails forever on the
// collection or index it created itself — the same class as the capped-size
// rounding above, and the reason both live here rather than in a comparison rule.
func isSimpleCollation(opt collectionOption, want any) bool {
	if opt.param != "collation" {
		return false
	}
	m, ok := asStringMap(want)
	if !ok {
		return false
	}
	locale, _ := m["locale"].(string)
	return locale == "simple"
}

// plural is a one-word grammar fix for the multi-field reports above.
func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// applyCollectionAbsent drops the collection. Idempotent through the same
// listCollections probe: one that is not there is a no-op.
//
// ★ This DESTROYS the collection's documents and its indexes. There is no
// confirmation flag, because this artifact has none anywhere and a flag that is
// always set is not a gate; what there is instead is this saying so plainly, and a
// scenario deciding when to run it.
func (m *MongoModule) applyCollectionAbsent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	db := stringOrEmpty(f["database"])
	name := stringOrEmpty(f["name"])

	live, err := readCollection(ctx, conn, db, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if !live.exists {
		return sendOutcome(stream, false, fmt.Sprintf("collection %s.%s already absent", db, name),
			map[string]any{"database": db, "name": name, "present": false})
	}
	if _, err := conn.RunCommand(ctx, db, bson.D{{Key: "drop", Value: name}}); err != nil {
		return sendFailure(stream, "drop: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("collection %s.%s dropped", db, name),
		map[string]any{"database": db, "name": name, "present": false})
}

// --- Validate ---

// validateCollectionPresent refuses what Apply would: the subject, and every
// declared option whose value is outside the closed set mongod accepts.
func validateCollectionPresent(f map[string]*structpb.Value) []string {
	errs := validateCollectionSubject(f)
	if _, err := declaredOptions(f); err != nil {
		errs = append(errs, err.Error())
	}
	if intOrDefault(f["size"], 1) < 0 {
		errs = append(errs, "params.size: must be >= 0 (bytes of a capped collection)")
	}
	if intOrDefault(f["max"], 1) < 0 {
		errs = append(errs, "params.max: must be >= 0 (documents in a capped collection)")
	}
	return errs
}

// validateCollectionAbsent — the subject alone.
func validateCollectionAbsent(f map[string]*structpb.Value) []string {
	return validateCollectionSubject(f)
}

// validateCollectionSubject is what both actions require: somewhere to connect to,
// the database, and the collection this step is about. `database` is REQUIRED and
// does not default to admin the way the `user` object's does — a collection in the
// admin database is almost never what an author meant, and defaulting there would
// silently create one.
func validateCollectionSubject(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "database")...)
	return append(errs, requireString(f, "name")...)
}
