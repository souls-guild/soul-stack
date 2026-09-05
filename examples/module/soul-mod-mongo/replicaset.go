// The `replicaset` object — a MongoDB replica set brought into being and kept at
// the declared membership, ENTIRELY through go-mongo-driver commands
// (replSetGetConfig / replSetGetStatus / replSetInitiate / replSetReconfig). No
// mongosh, no shell: the artifact's capability stays network_outbound.
//
// ★ THE SAFETY INVARIANT OF THIS FILE
//
// `replSetInitiate` on an already-initiated set and `replSetReconfig` on a live one
// are different operations with different costs, and the second one can trigger an
// election and break writes in flight. So this object never guesses which it is in:
// it ASKS ([readRSConfig]) and branches on the answer, exactly as the redis
// `cluster` object asks CLUSTER INFO / CLUSTER NODES before deciding whether to
// build, complete or no-op. An external probe plus a `when:` over it is the shape
// this deliberately does NOT take — a second guard of the same invariant drifts
// away from the first, and it cannot tell apart the two "not initiated" answers
// that matter most (see below).
//
// And the stronger half of the same invariant:
//
//	NO reconfig is ever ASSEMBLED FROM PARAMS. Every one of them is the LIVE
//	document from replSetGetConfig, mutated minimally, with version + 1.
//
// The live config is carried as the ordered [bson.D] mongod returned ([rsConfig]),
// and only `version` and `members` are ever replaced. Everything this artifact does
// not model — `settings`, `protocolVersion`, `term`,
// `writeConcernMajorityJournalDefault`, whatever a future server adds — rides
// through untouched, because there is no code path here that could drop it. An
// operator's out-of-band `settings` survives a member being added by us; a config
// rebuilt from params would have silently reset it.
//
// The `_id` of an existing member is likewise never reassigned: reusing one under a
// different host is a classic way to break a set. New members take max(_id)+1
// upward, in the sorted order of the keys in `params.members`, so the same input
// yields the same config — the determinism the redis cluster layout has for the
// same reason.
//
// ★ WHY THE TWO "NOT INITIATED" ANSWERS ARE DIFFERENT
//
// A mongod started WITHOUT `replication.replSetName` refuses these commands with
// NoReplicationEnabled (76); one started WITH it but never initiated refuses with
// NotYetInitialized (94). Only the second is ours to fix. The first is a config +
// restart away and the plugin says so by name — a `when: not initiated` guard reads
// both as "go ahead and initiate" and drives replSetInitiate into a mongod that
// will never accept it.
//
// ★ BOOTSTRAP
//
// With `security.authorization: enabled` a replica set is initiated BEFORE the first
// admin exists, because the localhost exception holds only while the admin DB has no
// users. So `initiated` takes the same connection path `user.present` takes —
// [MongoModule.openUserConn] with the bootstrap fallback — rather than the shared one
// in [object.Apply]. The day-2 actions run against an authenticated set and pass
// allowBootstrap=false.
//
// ★ THE PRIMARY HOP
//
// replSetReconfig must run on the primary. `replSetGetStatus` names it, but by its
// CONFIG host, which is routable between members and not necessarily from this host
// — the same split redis has between a node's `addr` (dial) and its `ip:port`
// (gossip). A member therefore declares `host` (what goes into the config) and an
// optional `addr` (what we dial), and the day-2 actions, which are given one member
// rather than the whole set, take an optional `primary_addr` for the same reason.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/protobuf/types/known/structpb"
)

// MongoDB server error codes this file branches on. They are the difference
// between an operation we can perform and one we must refuse by name, so they are
// matched on the typed [mongo.CommandError] first and on the codeName text only as
// a fallback — the same two-step [isAuthError] uses, and for the same reason: not
// every path returns a typed command error.
const (
	codeNamespaceNotFound    = 26
	codeNoReplicationEnabled = 76
	codeNotYetInitialized    = 94
)

// defaultMongoPort is what mongod appends to a `members[].host` written without
// one, so a desired host must be normalized the same way before it is compared
// with the live config ([normalizeHost]).
const defaultMongoPort = "27017"

// Waiting for a primary is a bounded poll, never an unbounded one: a set that has
// not elected within the operator's budget is NOT converged, and reporting it as
// reconciled is the lie this whole artifact is built to avoid.
const rsPollInterval = 200 * time.Millisecond

// defaultWaitPrimarySeconds is the budget when the operator names none. An election
// on a healthy set is seconds; a minute covers a slow one without hanging a run.
const defaultWaitPrimarySeconds = 60

// adminDB is where every replica-set command runs.
const adminDB = "admin"

// rsMemberAttrs are the member attributes this artifact models, in the spelling
// mongod uses in the config document. `secondaryDelaySecs` is the 5.0 name of what
// older servers called `slaveDelay`; this artifact does not serve the old spelling.
type rsMemberAttrs struct {
	priority           float64
	votes              int
	arbiterOnly        bool
	hidden             bool
	buildIndexes       bool
	secondaryDelaySecs int
	tags               map[string]string
}

// memberAttrKeys are the param spellings of [rsMemberAttrs], paired with the config
// spellings, in one place so the parser, the comparator and the document builder
// cannot drift apart. Sorted, because the built document must be byte-stable.
var memberAttrKeys = []struct{ param, field string }{
	{"arbiter_only", "arbiterOnly"},
	{"build_indexes", "buildIndexes"},
	{"hidden", "hidden"},
	{"priority", "priority"},
	{"secondary_delay_secs", "secondaryDelaySecs"},
	{"tags", "tags"},
	{"votes", "votes"},
}

// memberAttrParams names the attributes for the refusal message above.
func memberAttrParams() []string {
	out := make([]string, 0, len(memberAttrKeys))
	for _, k := range memberAttrKeys {
		out = append(out, k.param)
	}
	return out
}

// rsMember is one member as the OPERATOR declared it.
//
// given records which attribute keys were actually written, and it is load-bearing
// rather than bookkeeping: an attribute the operator did not name is one this step
// must not touch. Without it an omitted `priority` would compare as the default 1
// and "fix" a member deliberately pinned to 0 — a reconfig, and possibly an
// election, that nobody asked for.
type rsMember struct {
	key   string
	host  string
	addr  string
	attrs rsMemberAttrs
	given map[string]bool
}

// dialAddr is where this member is reached from THIS host: `addr` when the operator
// distinguished it from the config host, the host itself otherwise.
func (m rsMember) dialAddr() string {
	if m.addr != "" {
		return m.addr
	}
	return m.host
}

// rsConfig is the live replica-set configuration, kept as the ordered document
// mongod returned. doc is the whole of it; id, version and members are read out of
// it for the decisions above. Writing goes back through [rsConfig.with], which
// replaces exactly two fields and copies the rest verbatim.
type rsConfig struct {
	doc     bson.D
	id      string
	version int64
	members []bson.D
}

// with returns the live document with `members` replaced and `version` bumped, and
// every other field — modelled or not — carried across in its original position.
//
// This is the one function that builds a document for replSetReconfig, and it
// cannot produce one that was not derived from the live config. That is the point:
// the invariant at the top of this file is enforced by there being no other way.
func (c rsConfig) with(members []bson.D) bson.D {
	out := make(bson.D, 0, len(c.doc)+2)
	seenVersion, seenMembers := false, false
	for _, e := range c.doc {
		switch e.Key {
		case "version":
			out = append(out, bson.E{Key: "version", Value: c.version + 1})
			seenVersion = true
		case "members":
			out = append(out, bson.E{Key: "members", Value: membersToArray(members)})
			seenMembers = true
		default:
			out = append(out, e)
		}
	}
	// A live config always carries both, but a document that somehow does not
	// must still come out valid rather than half-written.
	if !seenVersion {
		out = append(out, bson.E{Key: "version", Value: c.version + 1})
	}
	if !seenMembers {
		out = append(out, bson.E{Key: "members", Value: membersToArray(members)})
	}
	return out
}

// membersToArray is the one place a members list becomes a bson array.
func membersToArray(members []bson.D) bson.A {
	arr := make(bson.A, 0, len(members))
	for _, m := range members {
		arr = append(arr, m)
	}
	return arr
}

// --- reading the live state ---

// rsState is what the live instance says about the set, and it is the whole of the
// decision this object makes.
type rsState int

const (
	// rsNotInitiated — the mongod is in replica-set mode and has no config yet
	// (NotYetInitialized). This is the ONLY state replSetInitiate is sent in.
	rsNotInitiated rsState = iota
	// rsLive — a config exists. What to do with it is then decided by comparing
	// it with the desired membership; nothing here initiates.
	rsLive
)

// readRSConfig asks the instance for its replica-set configuration and classifies
// the answer.
//
// NoReplicationEnabled is returned as an ERROR naming the cause, not as
// "not initiated": the plugin cannot add `replication.replSetName` to a running
// mongod, and saying "not initiated" here is what would send replSetInitiate at an
// instance that can only refuse it.
func readRSConfig(ctx context.Context, conn mongoConn) (rsState, rsConfig, error) {
	raw, err := conn.RunCommand(ctx, adminDB, bson.D{{Key: "replSetGetConfig", Value: 1}})
	if err != nil {
		switch {
		case isMongoCode(err, codeNoReplicationEnabled, "NoReplicationEnabled"):
			return 0, rsConfig{}, errors.New(
				"mongod is not running in replica-set mode (NoReplicationEnabled): " +
					"replication.replSetName must be set in mongod.conf and the instance restarted — " +
					"this plugin cannot change the server's command line")
		case isMongoCode(err, codeNotYetInitialized, "NotYetInitialized"):
			return rsNotInitiated, rsConfig{}, nil
		default:
			return 0, rsConfig{}, fmt.Errorf("replSetGetConfig: %w", err)
		}
	}

	cfg, err := parseRSConfig(raw)
	if err != nil {
		return 0, rsConfig{}, err
	}
	return rsLive, cfg, nil
}

// parseRSConfig lifts the `config` sub-document out of a replSetGetConfig reply and
// keeps it whole. Everything downstream reads from this value and writes through
// [rsConfig.with], so nothing that arrived here can be lost on the way back.
func parseRSConfig(raw bson.Raw) (rsConfig, error) {
	val, err := raw.LookupErr("config")
	if err != nil {
		return rsConfig{}, fmt.Errorf("replSetGetConfig: reply carries no config document")
	}
	sub, ok := val.DocumentOK()
	if !ok {
		return rsConfig{}, fmt.Errorf("replSetGetConfig: config is not a document")
	}

	var doc bson.D
	if err := bson.Unmarshal(sub, &doc); err != nil {
		return rsConfig{}, fmt.Errorf("replSetGetConfig: decode config: %w", err)
	}

	cfg := rsConfig{doc: doc}
	for _, e := range doc {
		switch e.Key {
		case "_id":
			if s, ok := e.Value.(string); ok {
				cfg.id = s
			}
		case "version":
			cfg.version = asInt64(e.Value)
		case "members":
			arr, ok := e.Value.(bson.A)
			if !ok {
				return rsConfig{}, fmt.Errorf("replSetGetConfig: members is not an array")
			}
			for _, m := range arr {
				md, ok := m.(bson.D)
				if !ok {
					return rsConfig{}, fmt.Errorf("replSetGetConfig: a member is not a document")
				}
				cfg.members = append(cfg.members, md)
			}
		}
	}
	if cfg.id == "" {
		return rsConfig{}, fmt.Errorf("replSetGetConfig: config carries no _id")
	}
	return cfg, nil
}

// memberHost / memberID read the two fields of a live member document this file
// makes decisions on. Both are absent-tolerant: a member document without them is
// reported by the caller, not guessed at here.
func memberHost(m bson.D) string {
	for _, e := range m {
		if e.Key == "host" {
			if s, ok := e.Value.(string); ok {
				return normalizeHost(s)
			}
		}
	}
	return ""
}

func memberID(m bson.D) int64 {
	for _, e := range m {
		if e.Key == "_id" {
			return asInt64(e.Value)
		}
	}
	return -1
}

// asInt64 normalizes the integer types bson can carry a number in. A config
// `version` arrives as int32 and a `_id` may arrive as either.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

// normalizeHost appends mongod's default port to a host written without one, so a
// desired `mongo-1` and a live `mongo-1:27017` are recognized as the same member.
// Without it every apply after the first would see a membership that "changed".
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(h, defaultMongoPort)
}

// --- replSetGetStatus ---

// rsStatusMember is one row of replSetGetStatus, reduced to what this file reads.
type rsStatusMember struct {
	name  string
	state string
}

// readRSStatus reports the live member states. It is used for exactly two things:
// finding the primary to send a reconfig to, and waiting for one to exist.
func readRSStatus(ctx context.Context, conn mongoConn) ([]rsStatusMember, error) {
	raw, err := conn.RunCommand(ctx, adminDB, bson.D{{Key: "replSetGetStatus", Value: 1}})
	if err != nil {
		return nil, err
	}
	val, lookupErr := raw.LookupErr("members")
	if lookupErr != nil {
		return nil, nil
	}
	arr, ok := val.ArrayOK()
	if !ok {
		return nil, nil
	}
	vals, err := arr.Values()
	if err != nil {
		return nil, nil
	}
	out := make([]rsStatusMember, 0, len(vals))
	for _, v := range vals {
		doc, ok := v.DocumentOK()
		if !ok {
			continue
		}
		row := rsStatusMember{}
		if nv, err := doc.LookupErr("name"); err == nil {
			row.name, _ = nv.StringValueOK()
		}
		if sv, err := doc.LookupErr("stateStr"); err == nil {
			row.state, _ = sv.StringValueOK()
		}
		out = append(out, row)
	}
	return out, nil
}

// primaryOf returns the config host of the member reporting PRIMARY, or "".
func primaryOf(rows []rsStatusMember) string {
	for _, r := range rows {
		if r.state == "PRIMARY" {
			return normalizeHost(r.name)
		}
	}
	return ""
}

// waitForPrimary polls until some member reports PRIMARY or the budget runs out. A
// budget of zero means "do not wait" and returns whatever a single read says.
//
// Running out is an ERROR: a replica set with no primary takes no writes, so a step
// that returned success here would be reporting a set as reconciled that cannot be
// used. That is the redis `failed-over` sync-gate rule — fail closed rather than
// escalate — applied to the one place mongo has the same choice.
// The failure carries the LAST error replSetGetStatus gave, when there was one.
// Without it a step authenticating as a user without `clusterMonitor` reports "no
// PRIMARY elected" about a perfectly healthy set — a false statement that hides a
// one-line grant, and the same misdiagnosis a transient network error would get.
func waitForPrimary(ctx context.Context, conn mongoConn, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		rows, err := readRSStatus(ctx, conn)
		if err == nil {
			if p := primaryOf(rows); p != "" {
				return p, nil
			}
			// Cleared, not kept: a poll that ANSWERED says the earlier failure was
			// transient, and holding on to it would rewrite the diagnosis of every
			// later poll. One network blip followed by three hundred clean reads of
			// a set that lost quorum must report the lost quorum, not the blip.
			lastErr = nil
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				// lastErr is cleared by any poll that answers, so a non-nil one
				// here means the LAST poll failed — not that none ever answered.
				// Claiming the latter sends an operator after a grant when the set
				// has really lost quorum and one late poll happened to error.
				return "", fmt.Errorf("no PRIMARY seen within %s, and the last replSetGetStatus failed (%w) — "+
					"if that error is a permission one, clusterMonitor is what it wants; "+
					"otherwise the set has no primary", budget, lastErr)
			}
			return "", fmt.Errorf("no PRIMARY elected within %s: the set is not writable", budget)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(rsPollInterval):
		}
	}
}

// --- desired membership ---

// parseMembers reads params.members into the deterministic desired set. Keys are
// sorted, so the `_id` a fresh set assigns and the order of every message below are
// reproducible from the input alone.
func parseMembers(v *structpb.Value, addr string) ([]rsMember, error) {
	spec := structField(v)
	if len(spec) == 0 {
		return nil, fmt.Errorf("%s: must be a non-empty map (key -> {host, ...})", addr)
	}
	out := make([]rsMember, 0, len(spec))
	for _, key := range sortedKeys(spec) {
		m, err := parseMember(spec[key], fmt.Sprintf("%s[%s]", addr, key), key)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// memberSpecKeys is every key a member spec may carry — the two addressing ones
// plus the param spelling of each attribute. It exists for [parseMember]'s
// unknown-key refusal below.
var memberSpecKeys = func() map[string]bool {
	keys := map[string]bool{"host": true, "addr": true}
	for _, k := range memberAttrKeys {
		keys[k.param] = true
	}
	return keys
}()

// parseMember reads ONE member spec. Every attribute goes through the strict
// readers in params.go: a value of the wrong type is refused here rather than
// falling back to a default, which for `priority` and `votes` is the difference
// between a member that cannot win an election and one that can.
//
// ★ AN UNKNOWN KEY IS REFUSED, and that is the NIM-800 rule carried one level down.
// The engine's `unknown_param` stops at the outer map — it has no declaration for
// what is INSIDE `members` — so without this an unrecognised key is silently
// dropped and the member is built without it. That is not hypothetical: three of
// the seven attributes are spelled differently here from the mongod config an
// author is reading (`arbiter_only` against `arbiterOnly`, `build_indexes` against
// `buildIndexes`, `secondary_delay_secs` against `slaveDelay`/`secondaryDelaySecs`),
// so `arbiterOnly: true` is the likely typo — and dropping it joins a full
// data-bearing secondary, which initial-syncs the whole dataset, where an arbiter
// was declared. Silently wrong and reported reconciled is exactly the shape
// NIM-778/NIM-800 exist to refuse.
func parseMember(v *structpb.Value, addr, key string) (rsMember, error) {
	spec := structField(v)
	if len(spec) == 0 {
		return rsMember{}, fmt.Errorf("%s: must be a map {host, ...}", addr)
	}
	var unknown []string
	for _, k := range sortedKeys(spec) {
		if !memberSpecKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return rsMember{}, fmt.Errorf(
			"%s: unknown member %s %s — this spec takes host, addr, %s "+
				"(note the snake_case: mongod's own arbiterOnly/buildIndexes/secondaryDelaySecs are spelled "+
				"arbiter_only/build_indexes/secondary_delay_secs here)",
			addr, plural("key", len(unknown)), strings.Join(unknown, ", "), strings.Join(memberAttrParams(), ", "))
	}

	host, err := stringField(spec, "host", addr+".host", "")
	if err != nil {
		return rsMember{}, err
	}
	if strings.TrimSpace(host) == "" {
		return rsMember{}, fmt.Errorf("%s.host: must be a non-empty string (host:port as the set members see it)", addr)
	}
	dial, err := stringField(spec, "addr", addr+".addr", "")
	if err != nil {
		return rsMember{}, err
	}

	m := rsMember{
		key:   key,
		host:  normalizeHost(host),
		addr:  strings.TrimSpace(dial),
		given: make(map[string]bool, len(memberAttrKeys)),
		attrs: rsMemberAttrs{priority: 1, votes: 1, buildIndexes: true},
	}
	for _, k := range memberAttrKeys {
		if _, ok := spec[k.param]; ok && !isNull(spec[k.param]) {
			m.given[k.param] = true
		}
	}

	if m.attrs.priority, err = numberField(spec, "priority", addr+".priority", 1); err != nil {
		return rsMember{}, err
	}
	if m.attrs.votes, err = intField(spec, "votes", addr+".votes", 1); err != nil {
		return rsMember{}, err
	}
	if m.attrs.arbiterOnly, err = boolField(spec, "arbiter_only", addr+".arbiter_only", false); err != nil {
		return rsMember{}, err
	}
	if m.attrs.hidden, err = boolField(spec, "hidden", addr+".hidden", false); err != nil {
		return rsMember{}, err
	}
	if m.attrs.buildIndexes, err = boolField(spec, "build_indexes", addr+".build_indexes", true); err != nil {
		return rsMember{}, err
	}
	if m.attrs.secondaryDelaySecs, err = intField(spec, "secondary_delay_secs", addr+".secondary_delay_secs", 0); err != nil {
		return rsMember{}, err
	}
	// mongod's default priority is 1 for an ordinary member and 0 for an arbiter —
	// which is why `rs.addArb()` takes no priority argument. Defaulting to 1 here
	// would make Validate refuse `{arbiter_only: true}` on its own, an input Apply
	// accepts happily because [memberDoc] writes only what was named and lets the
	// server fill the rest. Over-refusing is the other half of NIM-786.
	if m.attrs.arbiterOnly && !m.given["priority"] {
		m.attrs.priority = 0
	}

	tags, err := mapField(spec, "tags", addr+".tags")
	if err != nil {
		return rsMember{}, err
	}
	if tags != nil {
		m.attrs.tags = make(map[string]string, len(tags))
		for _, tk := range sortedKeys(tags) {
			tv, err := stringField(tags, tk, fmt.Sprintf("%s.tags[%s]", addr, tk), "")
			if err != nil {
				return rsMember{}, err
			}
			m.attrs.tags[tk] = tv
		}
	}
	return m, nil
}

// memberDoc builds the config document for a member being ADDED (by initiate or by
// a reconfig). Field order is fixed so the same input yields the same bytes.
//
// Only the attributes the operator NAMED are written. mongod fills the rest with
// its own defaults, which is a better answer than this artifact writing its idea of
// them into an operator's config.
func memberDoc(m rsMember, id int64) bson.D {
	doc := bson.D{{Key: "_id", Value: id}, {Key: "host", Value: m.host}}
	for _, k := range memberAttrKeys {
		if !m.given[k.param] {
			continue
		}
		doc = append(doc, bson.E{Key: k.field, Value: attrValue(m.attrs, k.param)})
	}
	return doc
}

// attrValue is the config value of one named attribute.
func attrValue(a rsMemberAttrs, param string) any {
	switch param {
	case "arbiter_only":
		return a.arbiterOnly
	case "build_indexes":
		return a.buildIndexes
	case "hidden":
		return a.hidden
	case "priority":
		return a.priority
	case "secondary_delay_secs":
		return int64(a.secondaryDelaySecs)
	case "tags":
		return tagsDoc(a.tags)
	case "votes":
		return int64(a.votes)
	default:
		return nil
	}
}

// tagsDoc renders member tags in sorted key order (byte-stable config).
func tagsDoc(tags map[string]string) bson.D {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	doc := make(bson.D, 0, len(keys))
	for _, k := range keys {
		doc = append(doc, bson.E{Key: k, Value: tags[k]})
	}
	return doc
}

// applyGivenAttrs returns a live member document with ONLY the named attributes
// replaced, and reports whether anything actually changed.
//
// The live document is the base and the caller's values are laid over it, so a
// field this artifact does not model survives; and a value that already matches
// produces no change, which is what makes `reconfigured` idempotent.
func applyGivenAttrs(live bson.D, m rsMember) (bson.D, bool) {
	out := append(bson.D(nil), live...)
	changed := false

	for _, k := range memberAttrKeys {
		if !m.given[k.param] {
			continue
		}
		want := attrValue(m.attrs, k.param)
		idx := -1
		for i, e := range out {
			if e.Key == k.field {
				idx = i
				break
			}
		}
		if idx < 0 {
			out = append(out, bson.E{Key: k.field, Value: want})
			changed = true
			continue
		}
		if !sameAttr(out[idx].Value, want) {
			out[idx].Value = want
			changed = true
		}
	}
	return out, changed
}

// memberMatchesDeclaration reports whether a live member already carries every
// attribute the declaration names, and is the ONE place that answers it.
//
// Both the seed-side check and the PRIMARY-SIDE RE-CHECK call it, and the second
// is the one worth naming: the re-read after the primary hop exists precisely
// because the seed's config can be stale, so that is exactly where a member joined
// out of band appears — and matching it on the host alone there would report a
// member able to win an election as reconciled with a declaration that says it
// cannot.
func memberMatchesDeclaration(live bson.D, m rsMember) bool {
	_, changed := applyGivenAttrs(live, m)
	return !changed
}

// declarationDriftRefusal is the message both call sites give, so the seed-side
// and primary-side answers cannot drift apart in wording either.
func declarationDriftRefusal(host, action string) string {
	return fmt.Sprintf(
		"member %s is in the set with different attributes than %s declares: this action does not rewrite an "+
			"existing member — a priority or votes change can force an election, so it belongs to "+
			"mongo.replicaset.reconfigured", host, action)
}

// sameAttr compares a live attribute value with a desired one across the numeric
// types bson may have carried it in. `priority: 0` read back as int32 must not look
// different from the float64 the operator wrote, or every apply would reconfig.
//
// Everything that is not a scalar goes through [sameValue], the package's canonical
// comparison. `tags` is why that matters: it is a document, mongod returns its keys
// in whatever order it stored them, and a positional comparison would call
// `{dc: east, zone: a}` different from `{zone: a, dc: east}` — failing `initiated`
// permanently on a member nothing is wrong with, and making `reconfigured` send a
// real reconfig, with its election risk, purely to reorder two keys.
func sameAttr(live, want any) bool {
	switch w := want.(type) {
	case bool:
		l, ok := live.(bool)
		return ok && l == w
	case int64:
		return isNumeric(live) && asFloat(live) == float64(w)
	case float64:
		return isNumeric(live) && asFloat(live) == w
	default:
		return sameValue(live, want)
	}
}

func isNumeric(v any) bool {
	switch v.(type) {
	case int, int32, int64, float32, float64:
		return true
	default:
		return false
	}
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

// --- error classification ---

// isMongoCode reports whether err is the named MongoDB server error. The typed
// [mongo.CommandError] is authoritative; the codeName text is a fallback for the
// paths that do not produce one, which is the same two-step [isAuthError] uses.
func isMongoCode(err error, code int32, name string) bool {
	if err == nil {
		return false
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		if cmdErr.Code == code {
			return true
		}
		if cmdErr.Name == name {
			return true
		}
	}
	return strings.Contains(err.Error(), name)
}

// --- connecting ---

// rsDialer opens a connection to ONE address with the step's credentials and TLS.
//
// It goes through [MongoModule.openUserConn], the localhost-exception path the
// `user` object already uses, because `initiated` runs before the first admin
// exists: with `security.authorization: enabled` the exception is the only way in
// while the admin DB is empty. Day-2 actions pass allowBootstrap=false — the set is
// authenticated by then, and an auth failure there is a failure, not a bootstrap.
type rsDialer func(ctx context.Context, addr string, allowBootstrap bool) (mongoConn, bool, error)

func (m *MongoModule) rsDialer(base connConfig) rsDialer {
	return func(ctx context.Context, addr string, allowBootstrap bool) (mongoConn, bool, error) {
		cfg := base
		cfg.addr = addr
		return m.openUserConn(ctx, cfg, allowBootstrap)
	}
}

// primaryDialAddr answers where to reach the primary, whose CONFIG host is what
// replSetGetStatus names. The operator's explicit override wins, then the
// host->addr mapping the members carry, then the config host itself — which is the
// right answer whenever the set's hosts are routable from this one.
func primaryDialAddr(primaryHost string, byHost map[string]string, override string) string {
	if a := strings.TrimSpace(override); a != "" {
		return a
	}
	if a := byHost[primaryHost]; a != "" {
		return a
	}
	return primaryHost
}

// primaryView opens the primary and reads the replica-set config THERE — the
// document a reconfig must be derived from — returning the connection, a closer for
// it, and that config.
//
// ★ WHY THE CONFIG IS RE-READ RATHER THAN CARRIED OVER
//
// The config read from `params.addr` is what CLASSIFIES the set, but it is the
// SEED's view and may be a version behind: a member added out of band a moment ago
// leaves the seed at version 5 while the primary already holds 6. A reconfig built
// on the seed's document then goes out as version 6 and mongod refuses it — "New
// replica set configuration version must be greater than the current one" — an
// opaque failure where a re-read simply converges. The header of this file says
// every reconfig is the LIVE document; this is what makes that the PRIMARY's live
// document rather than whichever member happened to answer first.
//
// allowBootstrap is the caller's, not a constant: `initiated` may run before the
// first admin exists, and hardcoding it off here refused the very case that action
// exists to serve — a partially built set on an auth-enabled mongod with an empty
// admin DB, where the hop is to the same instance under its config host.
//
// When the instance already open IS the primary, that connection is reused and the
// closer is a no-op — the caller's own defer still owns it.
func (m *MongoModule) primaryView(ctx context.Context, dial rsDialer, cur mongoConn, curAddr, primaryHost string,
	byHost map[string]string, override string, allowBootstrap bool) (mongoConn, func(), rsConfig, bool, error) {
	conn, closer, usedLocalhost := cur, func() {}, false

	if target := primaryDialAddr(primaryHost, byHost, override); normalizeHost(target) != normalizeHost(curAddr) {
		opened, viaLocalhost, err := dial(ctx, target, allowBootstrap)
		if err != nil {
			return nil, nil, rsConfig{}, false, fmt.Errorf("connect primary %s: %w", target, err)
		}
		conn, closer, usedLocalhost = opened, func() { _ = opened.Close(ctx) }, viaLocalhost
	}

	state, live, err := readRSConfig(ctx, conn)
	if err != nil {
		closer()
		return nil, nil, rsConfig{}, false, err
	}
	if state != rsLive {
		closer()
		return nil, nil, rsConfig{}, false, fmt.Errorf(
			"the primary %s reports no replica-set config: the set changed under this step, re-run it", primaryHost)
	}

	// ★ AND IT MUST STILL BE THE PRIMARY. An election runs on its own schedule, so
	// the node named by the seed's status can have handed over by the time this
	// connection opened — and a replSetReconfig sent down a secondary comes back as
	// NotWritablePrimary, the opaque failure this hop exists to avoid. Confirming
	// here covers every action at once: re-running converges, and the message says
	// what moved rather than leaving the operator with a server error.
	rows, err := readRSStatus(ctx, conn)
	if err != nil {
		closer()
		return nil, nil, rsConfig{}, false, fmt.Errorf("replSetGetStatus on %s: %w", primaryHost, err)
	}
	if now := primaryOf(rows); now != primaryHost {
		closer()
		return nil, nil, rsConfig{}, false, fmt.Errorf(
			"the PRIMARY moved from %s to %q while this step was connecting: nothing was written, re-run the step",
			primaryHost, now)
	}
	return conn, closer, live, usedLocalhost, nil
}

// waitBudget is the operator's primary-election budget as a duration. Zero is a
// legitimate value and means "read once, do not poll".
func waitBudget(f map[string]*structpb.Value) time.Duration {
	return time.Duration(intOrDefault(f["wait_primary_seconds"], defaultWaitPrimarySeconds)) * time.Second
}

// hostAddrMap is the host -> dial-address mapping the desired members carry, used
// to reach the primary named by replSetGetStatus.
func hostAddrMap(members []rsMember) map[string]string {
	out := make(map[string]string, len(members))
	for _, m := range members {
		out[m.host] = m.dialAddr()
	}
	return out
}

// reconfig sends ONE replSetReconfig, built by [rsConfig.with] and therefore
// derived from the live document. Nothing else in this file writes a config.
func reconfig(ctx context.Context, conn mongoConn, doc bson.D) error {
	_, err := conn.RunCommand(ctx, adminDB, bson.D{{Key: "replSetReconfig", Value: doc}})
	return err
}

// nextMemberID is max(_id)+1 over the live members — never an index into the
// array, which is the mistake worth naming: a member removed from the MIDDLE
// leaves a hole, and `len(members)` would fill it, handing a new host the identity
// a live member two rows down still holds.
//
// It does reuse the id of a member removed from the TOP of the range, which is what
// mongod's own `rs.add()` does and is the reason this is max+1 rather than a
// high-water mark: nothing in a replica-set config remembers a retired id, so there
// is no source for one that would not have to be invented.
func nextMemberID(members []bson.D) int64 {
	next := int64(0)
	for _, m := range members {
		if id := memberID(m); id >= next {
			next = id + 1
		}
	}
	return next
}

// --- replicaset.initiated ---

// applyRSInitiated brings the set to the declared membership, and is the action
// that decides between initiate, complete and no-op by ASKING (see the file header).
//
// It is ADDITIVE by construction. A member the live config has and params do not is
// refused rather than dropped, and an attribute that drifted on an existing member
// is refused rather than rewritten — both name the action that does the thing
// deliberately (`member-removed`, `reconfigured`). Silently dropping a member is
// how a set loses its majority; silently rewriting a priority is how it holds an
// election in a step the operator read as assembly.
func (m *MongoModule) applyRSInitiated(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	name := strings.TrimSpace(stringOrEmpty(f["name"]))

	members, err := parseMembers(f["members"], "params.members")
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	cfg, err := parseConnConfig(params)
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	dial := m.rsDialer(cfg)

	// The bootstrap fallback is allowed here and nowhere else in this object: this
	// is the action that runs before the first admin exists.
	conn, usedLocalhost, err := dial(ctx, cfg.addr, true)
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, secrets...))
	}
	defer func() { _ = conn.Close(ctx) }()

	state, live, err := readRSConfig(ctx, conn)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	budget := waitBudget(f)

	if state == rsNotInitiated {
		return m.initiateSet(ctx, stream, conn, name, members, budget, usedLocalhost, secrets)
	}

	if live.id != name {
		return sendFailure(stream, fmt.Sprintf(
			"the live replica set is named %q and params.name is %q: renaming a live set is not an operation "+
				"(check params.name, or point this step at the right instance)", live.id, name))
	}

	desired := make(map[string]rsMember, len(members))
	for _, mem := range members {
		desired[mem.host] = mem
	}
	liveByHost := make(map[string]bson.D, len(live.members))
	var extra []string
	for _, lm := range live.members {
		host := memberHost(lm)
		if host == "" {
			return sendFailure(stream, "replSetGetConfig: a live member carries no host")
		}
		liveByHost[host] = lm
		if _, want := desired[host]; !want {
			extra = append(extra, host)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return sendFailure(stream, fmt.Sprintf(
			"the live set holds %d member(s) params.members does not declare (%s): this action never drops a member — "+
				"remove them with mongo.replicaset.member-removed, or declare them here",
			len(extra), strings.Join(extra, ", ")))
	}

	// Attribute drift on a member that already exists. Reported all at once and in
	// a stable order, because fixing them one apply at a time is worse than being
	// told the whole list.
	var drifted []string
	var missing []rsMember
	for _, mem := range members {
		lm, ok := liveByHost[mem.host]
		if !ok {
			missing = append(missing, mem)
			continue
		}
		if _, changed := applyGivenAttrs(lm, mem); changed {
			drifted = append(drifted, mem.host)
		}
	}
	if len(drifted) > 0 {
		return sendFailure(stream, fmt.Sprintf(
			"member(s) %s are in the set with different attributes than params.members declares: this action never "+
				"rewrites an existing member — a priority or votes change can force an election, so it belongs to "+
				"mongo.replicaset.reconfigured", strings.Join(drifted, ", ")))
	}

	if len(missing) == 0 {
		primary, err := waitForPrimary(ctx, conn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("replica set %q already formed (no-op)", name), map[string]any{
			"set":            name,
			"members":        int64(len(live.members)),
			"members_added":  int64(0),
			"initiated":      false,
			"primary":        primary,
			"version":        live.version,
			"used_localhost": usedLocalhost,
		})
	}

	// Partial: add exactly the missing members ON TOP of the live document — the
	// PRIMARY's, re-read there, not the seed's copy that classified the set.
	// allowBootstrap stays true: this action can be the one running before the
	// first admin exists, and the hop is often to the same instance under its
	// config host.
	primaryHost, err := waitForPrimary(ctx, conn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	pconn, closePrimary, live, hopLocalhost, err := m.primaryView(ctx, dial, conn, cfg.addr, primaryHost,
		hostAddrMap(members), stringOrEmpty(f["primary_addr"]), true)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer closePrimary()
	usedLocalhost = usedLocalhost || hopLocalhost

	// ★ RE-CLASSIFY AGAINST THE PRIMARY'S DOCUMENT, IN FULL.
	//
	// The seed's copy decided WHETHER to act; this decides what is written, so every
	// check the seed-side gate made is made again here — extra members, attribute
	// drift, and which members are missing. Re-checking only the members the seed
	// called missing would leave the larger half unguarded: a member the seed saw
	// as matching, changed out of band a moment later, would be copied out of the
	// primary's document into the reconfig and the set reported as completed while
	// it holds what the declaration forbids.
	onPrimary := make(map[string]bson.D, len(live.members))
	for _, lm := range live.members {
		host := memberHost(lm)
		if host == "" {
			return sendFailure(stream, "replSetGetConfig: a live member carries no host")
		}
		onPrimary[host] = lm
	}
	var extraOnPrimary, driftedOnPrimary []string
	for host := range onPrimary {
		if _, want := desired[host]; !want {
			extraOnPrimary = append(extraOnPrimary, host)
		}
	}
	if len(extraOnPrimary) > 0 {
		sort.Strings(extraOnPrimary)
		return sendFailure(stream, fmt.Sprintf(
			"the primary holds %d member(s) params.members does not declare (%s): this action never drops a member — "+
				"remove them with mongo.replicaset.member-removed, or declare them here",
			len(extraOnPrimary), strings.Join(extraOnPrimary, ", ")))
	}
	for _, mem := range members {
		if lm, ok := onPrimary[mem.host]; ok && !memberMatchesDeclaration(lm, mem) {
			driftedOnPrimary = append(driftedOnPrimary, mem.host)
		}
	}
	if len(driftedOnPrimary) > 0 {
		sort.Strings(driftedOnPrimary)
		return sendFailure(stream, declarationDriftRefusal(strings.Join(driftedOnPrimary, ", "), "params.members"))
	}

	next := nextMemberID(live.members)
	newMembers := append([]bson.D(nil), live.members...)
	added := make([]string, 0, len(members))
	for _, mem := range members {
		// Missing ON THE PRIMARY, which is the only list that matters now: one the
		// seed called missing may have arrived, and it has just been checked to
		// have arrived as declared.
		if _, ok := onPrimary[mem.host]; ok {
			continue
		}
		newMembers = append(newMembers, memberDoc(mem, next))
		added = append(added, mem.host)
		next++
	}
	if len(added) == 0 {
		// Every missing member arrived between the seed read and the primary read.
		// Nothing to write, and saying so is more honest than a reconfig that only
		// bumps the version.
		primary, err := waitForPrimary(ctx, pconn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("replica set %q already formed (no-op)", name), map[string]any{
			"set":            name,
			"members":        int64(len(live.members)),
			"members_added":  int64(0),
			"initiated":      false,
			"primary":        primary,
			"version":        live.version,
			"used_localhost": usedLocalhost,
		})
	}

	if err := reconfig(ctx, pconn, live.with(newMembers)); err != nil {
		return sendFailure(stream, "replSetReconfig: "+redactError(err, secrets...))
	}

	primary, err := waitForPrimary(ctx, pconn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	return sendOutcome(stream, true,
		fmt.Sprintf("replica set %q completed: %d member(s) added (%s)", name, len(added), strings.Join(added, ", ")),
		map[string]any{
			"set":            name,
			"members":        int64(len(newMembers)),
			"members_added":  int64(len(added)),
			"initiated":      false,
			"primary":        primary,
			"version":        live.version + 1,
			"used_localhost": usedLocalhost,
		})
}

// initiateSet is the from-scratch half: the ONLY place replSetInitiate is sent, and
// it is reached only from the NotYetInitialized branch of [readRSConfig].
func (m *MongoModule) initiateSet(ctx context.Context, stream eventStream, conn mongoConn,
	name string, members []rsMember, budget time.Duration, usedLocalhost bool, secrets []string) error {
	docs := make([]bson.D, 0, len(members))
	for i, mem := range members {
		docs = append(docs, memberDoc(mem, int64(i)))
	}
	cfgDoc := bson.D{{Key: "_id", Value: name}, {Key: "members", Value: membersToArray(docs)}}

	if _, err := conn.RunCommand(ctx, adminDB, bson.D{{Key: "replSetInitiate", Value: cfgDoc}}); err != nil {
		return sendFailure(stream, "replSetInitiate: "+redactError(err, secrets...))
	}

	primary, err := waitForPrimary(ctx, conn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("replica set %q initiated with %d member(s)", name, len(members)),
		map[string]any{
			"set":            name,
			"members":        int64(len(members)),
			"members_added":  int64(len(members)),
			"initiated":      true,
			"primary":        primary,
			"version":        int64(1),
			"used_localhost": usedLocalhost,
		})
}

// --- replicaset.member-added ---

// applyRSMemberAdded joins ONE member to a formed set (day-2). Idempotent: a host
// already in the config is a no-op. The reconfig is the live document plus one
// member, version + 1 — it does not touch a single existing entry.
func (m *MongoModule) applyRSMemberAdded(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)

	member, err := parseMember(f["member"], "params.member", "member")
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	conn, dial, cfg, err := m.openRSDay2(ctx, params)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer func() { _ = conn.Close(ctx) }()

	state, live, err := readRSConfig(ctx, conn)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if state == rsNotInitiated {
		return sendFailure(stream, "the replica set is not initiated: build it with mongo.replicaset.initiated first")
	}

	budget := waitBudget(f)

	// A host already in the config is a no-op — but only when it is the member that
	// was DECLARED. Matching on the host alone would make `changed=false` a fact
	// about the hostname rather than about the declaration: a member joined here as
	// a hidden non-voter, later given priority 1 out of band, would keep reporting
	// "already in the set" while it is now able to win an election. So the declared
	// attributes are compared, and a difference is refused by name — the same answer
	// `initiated` gives, and for the same reason.
	for _, lm := range live.members {
		if memberHost(lm) != member.host {
			continue
		}
		if !memberMatchesDeclaration(lm, member) {
			return sendFailure(stream, declarationDriftRefusal(member.host, "params.member"))
		}
		primary, err := waitForPrimary(ctx, conn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("member %s already in the set (no-op)", member.host),
			map[string]any{"host": member.host, "members": int64(len(live.members)),
				"primary": primary, "version": live.version})
	}

	primaryHost, err := waitForPrimary(ctx, conn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	pconn, closePrimary, live, _, err := m.primaryView(ctx, dial, conn, cfg.addr, primaryHost,
		hostAddrMap([]rsMember{member}), stringOrEmpty(f["primary_addr"]), false)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer closePrimary()

	// The primary may already hold the member: it can have arrived between the two
	// reads, and joining it twice would put one host in the config twice.
	for _, lm := range live.members {
		if memberHost(lm) != member.host {
			continue
		}
		if !memberMatchesDeclaration(lm, member) {
			return sendFailure(stream, declarationDriftRefusal(member.host, "params.member"))
		}
		primary, err := waitForPrimary(ctx, pconn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("member %s already in the set (no-op)", member.host),
			map[string]any{"host": member.host, "members": int64(len(live.members)),
				"primary": primary, "version": live.version})
	}

	newMembers := append(append([]bson.D(nil), live.members...), memberDoc(member, nextMemberID(live.members)))
	if err := reconfig(ctx, pconn, live.with(newMembers)); err != nil {
		return sendFailure(stream, "replSetReconfig: "+redactError(err, secrets...))
	}
	primary, err := waitForPrimary(ctx, pconn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("member %s added to the set", member.host), map[string]any{
		"host":    member.host,
		"members": int64(len(newMembers)),
		"primary": primary,
		"version": live.version + 1,
	})
}

// --- replicaset.member-removed ---

// applyRSMemberRemoved evicts ONE member (day-2). Idempotent: a host the config does
// not hold is a no-op.
//
// Two things it refuses rather than performs. Removing the CURRENT PRIMARY forces an
// election, so it is refused with the command that does it deliberately; and a
// removal that would leave the set without an electable voting member is refused,
// because the result is a set that cannot elect and therefore cannot take a write.
func (m *MongoModule) applyRSMemberRemoved(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	host := normalizeHost(stringOrEmpty(f["host"]))

	conn, dial, cfg, err := m.openRSDay2(ctx, params)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer func() { _ = conn.Close(ctx) }()

	state, live, err := readRSConfig(ctx, conn)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if state == rsNotInitiated {
		return sendFailure(stream, "the replica set is not initiated: there is nothing to remove a member from")
	}

	kept := make([]bson.D, 0, len(live.members))
	found := false
	for _, lm := range live.members {
		if memberHost(lm) == host {
			found = true
			continue
		}
		kept = append(kept, lm)
	}
	budget := waitBudget(f)
	if !found {
		// The wait runs on the no-op path too, because the state's own description
		// promises it and a set with no primary is not one to report as fine.
		primary, err := waitForPrimary(ctx, conn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("member %s is not in the set (no-op)", host), map[string]any{
			"host": host, "members": int64(len(live.members)), "primary": primary, "version": live.version,
		})
	}

	primaryHost, err := waitForPrimary(ctx, conn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if primaryHost == host {
		return sendFailure(stream, fmt.Sprintf(
			"member %s is the current PRIMARY: removing it by reconfig forces an election. Step it down first "+
				"(mongo.command.run with { replSetStepDown: <seconds> }) and run this step again", host))
	}
	if n := electableVoters(kept); n == 0 {
		return sendFailure(stream, fmt.Sprintf(
			"removing %s would leave the set with no electable voting member (votes > 0 and priority > 0): "+
				"the result could not elect a primary and could not take a write", host))
	}

	pconn, closePrimary, live, _, err := m.primaryView(ctx, dial, conn, cfg.addr, primaryHost,
		nil, stringOrEmpty(f["primary_addr"]), false)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer closePrimary()

	// Recompute against the primary's document, and re-run the electable gate on
	// it: the seed's copy decided WHETHER to act, but what gets written must be
	// derived from — and checked against — the config actually being replaced.
	kept = kept[:0]
	found = false
	for _, lm := range live.members {
		if memberHost(lm) == host {
			found = true
			continue
		}
		kept = append(kept, lm)
	}
	if !found {
		primary, err := waitForPrimary(ctx, pconn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, fmt.Sprintf("member %s is not in the set (no-op)", host), map[string]any{
			"host": host, "members": int64(len(live.members)), "primary": primary, "version": live.version,
		})
	}
	if n := electableVoters(kept); n == 0 {
		return sendFailure(stream, fmt.Sprintf(
			"removing %s would leave the set with no electable voting member (votes > 0 and priority > 0): "+
				"the result could not elect a primary and could not take a write", host))
	}

	// The "is it the PRIMARY" gate needs no second poll here: [primaryView] refuses
	// outright if the primary moved between the seed's status read and this
	// connection, so reaching this point means the host being removed is still not
	// the one leading. A third full wait would also have tripled the operator's
	// declared budget.

	if err := reconfig(ctx, pconn, live.with(kept)); err != nil {
		return sendFailure(stream, "replSetReconfig: "+redactError(err, secrets...))
	}
	primary, err := waitForPrimary(ctx, pconn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("member %s removed from the set", host), map[string]any{
		"host":    host,
		"members": int64(len(kept)),
		"primary": primary,
		"version": live.version + 1,
	})
}

// electableVoters counts the live members that could win an election — votes > 0 and
// priority > 0, with mongod's defaults (1 and 1) for a member that names neither.
func electableVoters(members []bson.D) int {
	n := 0
	for _, m := range members {
		votes, priority := 1.0, 1.0
		for _, e := range m {
			switch e.Key {
			case "votes":
				votes = asFloat(e.Value)
			case "priority":
				priority = asFloat(e.Value)
			}
		}
		if votes > 0 && priority > 0 {
			n++
		}
	}
	return n
}

// --- replicaset.reconfigured ---

// applyRSReconfigured is the deliberate one: it changes attributes of members that
// ALREADY exist, which is the operation that can force an election. It is a separate
// action for exactly that reason — `initiated` refuses this work and names this
// action, so nothing here happens in a step an operator read as assembly.
//
// It is a PATCH, not a replacement. Only the attributes named in params.members are
// laid over the live member documents; the rest of each document, and every member
// not named, are carried across untouched. A run where nothing differs is a no-op.
func (m *MongoModule) applyRSReconfigured(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)

	members, err := parseMembers(f["members"], "params.members")
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	conn, dial, cfg, err := m.openRSDay2(ctx, params)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer func() { _ = conn.Close(ctx) }()

	state, live, err := readRSConfig(ctx, conn)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if state == rsNotInitiated {
		return sendFailure(stream, "the replica set is not initiated: build it with mongo.replicaset.initiated first")
	}

	byHost := make(map[string]int, len(live.members))
	for i, lm := range live.members {
		byHost[memberHost(lm)] = i
	}
	var unknown []string
	for _, mem := range members {
		if _, ok := byHost[mem.host]; !ok {
			unknown = append(unknown, mem.host)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return sendFailure(stream, fmt.Sprintf(
			"member(s) %s are not in the set: this action only changes members that already exist — "+
				"join them with mongo.replicaset.member-added", strings.Join(unknown, ", ")))
	}

	budget := waitBudget(f)
	if !patchChangesAnything(live, byHost, members) {
		primary, err := waitForPrimary(ctx, conn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, "every declared member already carries the declared attributes (no-op)",
			map[string]any{"members": int64(len(live.members)), "members_changed": int64(0),
				"primary": primary, "version": live.version})
	}

	primaryHost, err := waitForPrimary(ctx, conn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	pconn, closePrimary, live, _, err := m.primaryView(ctx, dial, conn, cfg.addr, primaryHost,
		hostAddrMap(members), stringOrEmpty(f["primary_addr"]), false)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	defer closePrimary()

	// The patch is applied to the PRIMARY's document, so the members are located in
	// it afresh: the seed's copy decided WHETHER to act, and this decides what is
	// written. A member that left the set between the two reads is refused rather
	// than re-added by an action whose whole contract is that it only changes what
	// already exists.
	byHost = make(map[string]int, len(live.members))
	for i, lm := range live.members {
		byHost[memberHost(lm)] = i
	}
	unknown = nil
	for _, mem := range members {
		if _, ok := byHost[mem.host]; !ok {
			unknown = append(unknown, mem.host)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return sendFailure(stream, fmt.Sprintf(
			"member(s) %s are not in the set: this action only changes members that already exist — "+
				"join them with mongo.replicaset.member-added", strings.Join(unknown, ", ")))
	}

	newMembers := append([]bson.D(nil), live.members...)
	var touched []string
	for _, mem := range members {
		i := byHost[mem.host]
		doc, changed := applyGivenAttrs(newMembers[i], mem)
		if changed {
			newMembers[i] = doc
			touched = append(touched, mem.host)
		}
	}
	if len(touched) == 0 {
		primary, err := waitForPrimary(ctx, pconn, budget)
		if err != nil {
			return sendFailure(stream, redactError(err, secrets...))
		}
		return sendOutcome(stream, false, "every declared member already carries the declared attributes (no-op)",
			map[string]any{"members": int64(len(live.members)), "members_changed": int64(0),
				"primary": primary, "version": live.version})
	}

	if err := reconfig(ctx, pconn, live.with(newMembers)); err != nil {
		return sendFailure(stream, "replSetReconfig: "+redactError(err, secrets...))
	}
	primary, err := waitForPrimary(ctx, pconn, budget)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	sort.Strings(touched)
	return sendOutcome(stream, true,
		fmt.Sprintf("reconfigured %d member(s): %s", len(touched), strings.Join(touched, ", ")),
		map[string]any{
			"members":         int64(len(newMembers)),
			"members_changed": int64(len(touched)),
			"primary":         primary,
			"version":         live.version + 1,
		})
}

// patchChangesAnything answers whether the declared patch would change ANY member
// of this config — the cheap pre-check that keeps a converged `reconfigured` from
// opening a second connection to the primary at all.
//
// The authoritative answer is computed again on the primary's own document, since
// that is what gets written; this one only decides whether it is worth going there.
func patchChangesAnything(live rsConfig, byHost map[string]int, members []rsMember) bool {
	for _, mem := range members {
		i, ok := byHost[mem.host]
		if !ok {
			continue
		}
		if _, changed := applyGivenAttrs(live.members[i], mem); changed {
			return true
		}
	}
	return false
}

// openRSDay2 is the connection every day-2 replicaset action opens: the operator's
// `addr`, WITHOUT the bootstrap fallback. These run against a set that already has
// an admin, so an auth failure is a failure and not a first-admin case — the same
// split `user.absent` makes against `user.present`.
func (m *MongoModule) openRSDay2(ctx context.Context, params *structpb.Struct) (mongoConn, rsDialer, connConfig, error) {
	cfg, err := parseConnConfig(params)
	if err != nil {
		return nil, nil, connConfig{}, err
	}
	dial := m.rsDialer(cfg)
	conn, _, err := dial(ctx, cfg.addr, false)
	if err != nil {
		return nil, nil, connConfig{}, fmt.Errorf("connect: %s", redactError(err, paramSecrets(params.GetFields())...))
	}
	return conn, dial, cfg, nil
}

// --- Validate ---
//
// Everything mongod enforces about a member document that is visible in the params
// is refused HERE, before anything happens (NIM-786). The point of the phase is to
// say no before a run is half done, and a rule it lets through silently is a rule
// the operator meets in the middle of a reconfig.

// validateRSInitiated — addr, the set name, a parseable non-empty membership, and
// the whole-set rules (a set is being described in full here, so they apply).
func validateRSInitiated(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "name")...)
	errs = append(errs, validateWaitPrimary(f)...)

	members, err := parseMembers(f["members"], "params.members")
	if err != nil {
		return append(errs, err.Error())
	}
	for _, mem := range members {
		errs = append(errs, validateMemberSpec(mem, "params.members["+mem.key+"]", true)...)
	}
	return append(errs, validateMemberSet(members)...)
}

// validateRSMemberAdded — one whole member, so its own rules apply with mongod's
// defaults filled in. The whole-set rules do not: the set already exists and its
// other members are not visible from here.
func validateRSMemberAdded(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, validateWaitPrimary(f)...)

	member, err := parseMember(f["member"], "params.member", "member")
	if err != nil {
		return append(errs, err.Error())
	}
	return append(errs, validateMemberSpec(member, "params.member", true)...)
}

// validateRSMemberRemoved — addr and the host being evicted. Whether it is the
// primary, and whether the set survives losing it, are live facts and are refused in
// Apply, which is where they first become knowable.
func validateRSMemberRemoved(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "host")...)
	return append(errs, validateWaitPrimary(f)...)
}

// validateRSReconfigured — a PATCH over existing members, so the rules are checked
// only where both halves of one are named. `hidden: true` with no `priority` beside
// it is not an error here: the live config may already hold priority 0, which is not
// visible from a params map, and refusing it would refuse input Apply accepts.
func validateRSReconfigured(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, validateWaitPrimary(f)...)

	members, err := parseMembers(f["members"], "params.members")
	if err != nil {
		return append(errs, err.Error())
	}
	for _, mem := range members {
		errs = append(errs, validateMemberSpec(mem, "params.members["+mem.key+"]", false)...)
	}
	return append(errs, validateDistinctHosts(members)...)
}

// validateWaitPrimary — the election budget is a duration, so a negative one is not
// a shorter wait, it is a wait that has already expired.
func validateWaitPrimary(f map[string]*structpb.Value) []string {
	if v, ok := f["wait_primary_seconds"]; ok && v != nil && !isNull(v) {
		if intOrDefault(v, defaultWaitPrimarySeconds) < 0 {
			return []string{"params.wait_primary_seconds: must be >= 0 (0 means do not wait)"}
		}
	}
	return nil
}

// validateMemberSpec reports what mongod would refuse about ONE member.
//
// withDefaults is the difference between an action that writes a WHOLE member
// (initiated, member-added — an attribute the operator omitted takes mongod's
// documented default, so a rule spanning it is decidable here) and one that writes a
// PATCH (reconfigured — an omitted attribute keeps whatever the live config holds,
// which is not visible from params, so that rule is left to the server rather than
// guessed at).
func validateMemberSpec(m rsMember, addr string, withDefaults bool) []string {
	var errs []string
	known := func(param string) bool { return withDefaults || m.given[param] }

	if known("votes") && m.attrs.votes != 0 && m.attrs.votes != 1 {
		errs = append(errs, fmt.Sprintf("%s.votes: must be 0 or 1, got %d (mongod allows no other value)", addr, m.attrs.votes))
	}
	if known("secondary_delay_secs") && m.attrs.secondaryDelaySecs < 0 {
		errs = append(errs, fmt.Sprintf("%s.secondary_delay_secs: must be >= 0", addr))
	}
	if known("priority") && m.attrs.priority < 0 {
		errs = append(errs, fmt.Sprintf("%s.priority: must be >= 0", addr))
	}

	// Every rule below is "X requires priority 0" — a member that can win an
	// election must be a plain, up-to-date, indexed, voting one.
	zeroPriority := []struct {
		param, why string
		on         bool
	}{
		{"hidden", "a hidden member", m.attrs.hidden},
		{"arbiter_only", "an arbiter", m.attrs.arbiterOnly},
		{"secondary_delay_secs", "a delayed member", m.attrs.secondaryDelaySecs > 0},
		{"build_indexes", "a member that does not build indexes", known("build_indexes") && !m.attrs.buildIndexes},
	}
	for _, r := range zeroPriority {
		if !known(r.param) || !r.on {
			continue
		}
		if known("priority") && m.attrs.priority != 0 {
			errs = append(errs, fmt.Sprintf("%s: %s must have priority 0, got %v", addr, r.why, m.attrs.priority))
		}
	}
	if known("votes") && m.attrs.votes == 0 && known("priority") && m.attrs.priority != 0 {
		errs = append(errs, fmt.Sprintf("%s: a non-voting member (votes 0) must have priority 0, got %v", addr, m.attrs.priority))
	}
	if known("arbiter_only") && m.attrs.arbiterOnly && known("votes") && m.attrs.votes != 1 {
		errs = append(errs, fmt.Sprintf("%s: an arbiter must have votes 1, got %d", addr, m.attrs.votes))
	}
	return errs
}

// validateMemberSet reports what mongod would refuse about the set AS A WHOLE. Only
// an action that describes the whole membership can be held to these.
func validateMemberSet(members []rsMember) []string {
	errs := validateDistinctHosts(members)

	if len(members) > 50 {
		errs = append(errs, fmt.Sprintf("params.members: a replica set holds at most 50 members, got %d", len(members)))
	}
	votes, electable := 0, 0
	for _, m := range members {
		votes += m.attrs.votes
		if m.attrs.votes > 0 && m.attrs.priority > 0 {
			electable++
		}
	}
	if votes > 7 {
		errs = append(errs, fmt.Sprintf("params.members: a replica set holds at most 7 VOTING members, got %d", votes))
	}
	if electable == 0 {
		errs = append(errs, "params.members: no member can be elected primary (every one has votes 0 or priority 0) — "+
			"such a set cannot take a write")
	}
	return errs
}

// validateDistinctHosts — two members on one host is a config mongod refuses, and
// the duplicate is invisible in a params map whose KEYS are distinct.
func validateDistinctHosts(members []rsMember) []string {
	seen := make(map[string][]string, len(members))
	for _, m := range members {
		seen[m.host] = append(seen[m.host], m.key)
	}
	var errs []string
	for _, host := range sortedStringKeys(seen) {
		if keys := seen[host]; len(keys) > 1 {
			errs = append(errs, fmt.Sprintf("params.members: host %s is declared by %d entries (%s) — one member per host",
				host, len(keys), strings.Join(keys, ", ")))
		}
	}
	return errs
}

// sortedStringKeys — deterministic order of error messages.
func sortedStringKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
