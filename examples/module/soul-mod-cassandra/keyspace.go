// The `keyspace` object's implementation — CREATE / ALTER / DROP KEYSPACE.
//
// The reconciling half follows the redis `cluster` shape: read the live schema
// first, then act on one of three answers (schema.go). Absent → CREATE. Matching →
// no-op. Differing → ALTER to the declared replication, touching nothing else.
//
// ★ REPAIR IS NOT PART OF THIS STATE, and the decision is not a matter of taste.
// Raising a replication factor changes the SCHEMA; it does not move a single row.
// The new replicas hold nothing until `nodetool repair` has run, and there is no CQL
// statement for repair — it is a JMX/nodetool operation, node-local, and on real
// data it runs for hours. An artifact whose whole contract is "CQL through the native
// driver, no shell" cannot perform it, and a state that blocked for hours inside one
// Apply would not be a state. So the state converges the DECLARED replication and
// says so out loud: the description says it, and Output.repair_required carries it to
// the scenario, which is where the follow-up belongs.
package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// The two replication strategies this artifact manages. Anything else is refused
// rather than passed through: an unknown strategy carries options of unknown type,
// and rendering them by guess is how a keyspace ends up with a replication an
// operator did not write.
const (
	simpleStrategy  = "SimpleStrategy"
	networkStrategy = "NetworkTopologyStrategy"
)

// classPrefix is what Cassandra prepends when it stores a strategy name, so the
// short name an author writes and the name that comes back are the same value.
const classPrefix = "org.apache.cassandra.locator."

// replicationFactorKey is SimpleStrategy's single option, and also the cluster-wide
// shorthand NetworkTopologyStrategy accepts.
const replicationFactorKey = "replication_factor"

// dcNameRe admits a datacenter name into a quoted CQL string literal. Looser than
// [identifierRe] on purpose — a real datacenter is called `us-east-1` or `DC1`, and
// neither is a CQL identifier — but a quote or a backslash, the two characters that
// could end the literal early, are outside the set.
var dcNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// replicationSpec is a keyspace's replication, in the one shape both the declared
// and the live side are normalized to before they are compared.
type replicationSpec struct {
	// class is the SHORT strategy name, with Cassandra's package prefix stripped.
	class string
	// factors is the option map: `replication_factor` for SimpleStrategy, one entry
	// per datacenter for NetworkTopologyStrategy.
	factors map[string]int
}

// keyspaceState is the live keyspace as system_schema reports it.
type keyspaceState struct {
	replication   replicationSpec
	durableWrites bool
}

const keyspaceQuery = `SELECT durable_writes, replication FROM system_schema.keyspaces WHERE keyspace_name = ?`

// parseReplication reads `params.replication` and refuses what it cannot render.
//
// Factors are declared as INTEGERS even though Cassandra stores them as text: a
// replication factor is a count, and admitting `"3"` beside `3` would be the same
// coercion params.go exists to refuse. A whole-cell `${ vars.rf }` over an integer
// var arrives as an integer (ADR-010), so the strict reading costs an author nothing.
func parseReplication(v *structpb.Value) (replicationSpec, error) {
	fields := mapField(v)
	if len(fields) == 0 {
		return replicationSpec{}, fmt.Errorf("params.replication: must be a non-empty map, e.g. {class: %s, dc1: 3}", networkStrategy)
	}

	classValue, ok := stringValue(fields["class"])
	if !ok || strings.TrimSpace(classValue) == "" {
		return replicationSpec{}, fmt.Errorf("params.replication.class: must be a non-empty string (%s or %s)", simpleStrategy, networkStrategy)
	}
	class := strings.TrimPrefix(strings.TrimSpace(classValue), classPrefix)
	if class != simpleStrategy && class != networkStrategy {
		return replicationSpec{}, fmt.Errorf("params.replication.class: %q is not a strategy this plugin manages (expected %s or %s)", classValue, simpleStrategy, networkStrategy)
	}

	spec := replicationSpec{class: class, factors: map[string]int{}}
	for _, key := range sortedKeys(fields) {
		if key == "class" {
			continue
		}
		if key != replicationFactorKey && !dcNameRe.MatchString(key) {
			return replicationSpec{}, fmt.Errorf("params.replication.%s: %q is not a usable datacenter name — letters, digits, dot, dash and underscore only", key, key)
		}
		n, isNumber := fields[key].GetKind().(*structpb.Value_NumberValue)
		if !isNumber || n.NumberValue != float64(int64(n.NumberValue)) {
			return replicationSpec{}, fmt.Errorf("params.replication.%s: must be an integer replication factor, got %s", key, valueTypeName(fields[key]))
		}
		if n.NumberValue < 0 {
			return replicationSpec{}, fmt.Errorf("params.replication.%s: replication factor must not be negative, got %d", key, int(n.NumberValue))
		}
		spec.factors[key] = int(n.NumberValue)
	}

	switch spec.class {
	case simpleStrategy:
		if _, has := spec.factors[replicationFactorKey]; !has || len(spec.factors) != 1 {
			return replicationSpec{}, fmt.Errorf("params.replication: %s takes exactly one option, %s", simpleStrategy, replicationFactorKey)
		}
	case networkStrategy:
		if len(spec.factors) == 0 {
			return replicationSpec{}, fmt.Errorf("params.replication: %s takes at least one datacenter, e.g. {class: %s, dc1: 3}", networkStrategy, networkStrategy)
		}
	}
	return spec, nil
}

// liveReplication normalizes what system_schema reports into the same shape
// [parseReplication] produces, so the comparison is between two values of one type
// and not between two spellings.
func liveReplication(raw map[string]string) (replicationSpec, error) {
	spec := replicationSpec{class: strings.TrimPrefix(raw["class"], classPrefix), factors: map[string]int{}}
	for key, value := range raw {
		if key == "class" {
			continue
		}
		n, err := atoiStrict("system_schema.keyspaces.replication."+key, value)
		if err != nil {
			return replicationSpec{}, err
		}
		spec.factors[key] = n
	}
	return spec, nil
}

// literal renders the CQL map. Deterministic: class first, then the options sorted —
// the same input always produces the same statement, which is what makes a diff
// between two runs mean something.
func (r replicationSpec) literal() string {
	parts := make([]string, 0, len(r.factors)+1)
	parts = append(parts, fmt.Sprintf("'class': '%s'", r.class))
	for _, key := range sortedKeys(r.factors) {
		parts = append(parts, fmt.Sprintf("'%s': '%d'", key, r.factors[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// summary is the same value for Output — readable, deterministic, no secrets.
func (r replicationSpec) summary() string {
	parts := make([]string, 0, len(r.factors)+1)
	parts = append(parts, r.class)
	for _, key := range sortedKeys(r.factors) {
		parts = append(parts, key+"="+strconv.Itoa(r.factors[key]))
	}
	return strings.Join(parts, ",")
}

func (r replicationSpec) equal(other replicationSpec) bool {
	if r.class != other.class || len(r.factors) != len(other.factors) {
		return false
	}
	for key, n := range r.factors {
		if other.factors[key] != n {
			return false
		}
	}
	return true
}

// raisesOver reports whether moving from live to r puts data on replicas that do not
// have it yet — the one case that owes a repair. A factor that only goes DOWN owes a
// `nodetool cleanup` instead, which is not a correctness problem: the surplus copies
// are still correct, merely surplus.
//
// A STRATEGY CHANGE always owes one. The two strategies do not share an option
// vocabulary — SimpleStrategy counts replicas cluster-wide, NetworkTopologyStrategy
// counts them per datacenter — so their factors are not comparable and which nodes
// hold a given partition changes regardless of the numbers. Comparing key by key
// across a class change was worse than useless: live NTS{dc1: 3} against declared
// Simple{replication_factor: 1} read the absent `replication_factor` as 0, called
// 1 > 0 a rise, and raised the banner on a change that strictly REDUCES replication.
// A flag that fires on non-events is one operators learn to ignore.
func (r replicationSpec) raisesOver(live replicationSpec) bool {
	if r.class != live.class {
		return true
	}
	for key, n := range r.factors {
		if n > live.factors[key] {
			return true
		}
	}
	return false
}

// readKeyspace reads the live keyspace. found=false means no such keyspace, which is
// an answer and not an error.
func readKeyspace(ctx context.Context, s cassSession, name string) (keyspaceState, bool, error) {
	rows, err := s.Query(ctx, keyspaceQuery, name)
	if err != nil {
		return keyspaceState{}, false, fmt.Errorf("read system_schema.keyspaces: %w", err)
	}
	if len(rows) == 0 {
		return keyspaceState{}, false, nil
	}
	repl, err := liveReplication(rowTextMap(rows[0], "replication"))
	if err != nil {
		return keyspaceState{}, false, err
	}
	return keyspaceState{replication: repl, durableWrites: rowBool(rows[0], "durable_writes")}, true, nil
}

// applyKeyspacePresent converges a keyspace to the declared replication.
func (m *CassandraModule) applyKeyspacePresent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	name := stringOrEmpty(f["name"])
	if err := checkIdentifier("params.name", name); err != nil {
		return sendFailure(stream, err.Error())
	}
	want, err := parseReplication(f["replication"])
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	durable := boolOrDefault(f["durable_writes"], true)

	live, found, err := readKeyspace(ctx, s, name)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	switch keyspaceConverge(found, live, want, durable) {
	case convergeAbsent:
		// IF NOT EXISTS, and not for tidiness: the converge read above went to ONE
		// coordinator, and a coordinator lagging behind a keyspace another node
		// already has would answer "absent" and turn this into a hard AlreadyExists.
		// The guard against that has to be in the statement, because the read cannot
		// see what it has not received.
		stmt := fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH REPLICATION = %s AND DURABLE_WRITES = %t", name, want.literal(), durable)
		if err := s.Exec(ctx, stmt); err != nil {
			return sendFailure(stream, "CREATE KEYSPACE: "+redactError(err, password))
		}
		if err := awaitSchema(ctx, s, "keyspace "+name); err != nil {
			return sendFailure(stream, redactError(err, password))
		}

		// ★ RE-READ, because IF NOT EXISTS turned the failure mode into a silent one.
		// If the keyspace did exist cluster-wide and only this coordinator was behind,
		// the CREATE was a server-side NO-OP: without this read the state would report
		// "created" with the declared replication and repair_required=false, while the
		// keyspace still carries its old replication and holds data. That is a wrong
		// answer on both fields, and it is the price of IF NOT EXISTS — paid for here
		// rather than left in. Before the guard this race was a loud AlreadyExists.
		live, found, err = readKeyspace(ctx, s, name)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		if !found {
			return sendFailure(stream, fmt.Sprintf("keyspace %s is still absent after CREATE and schema agreement", name))
		}
		if want.equal(live.replication) && durable == live.durableWrites {
			// A keyspace created now holds no data, so its replicas are not behind:
			// there is nothing to repair yet.
			return sendOutcome(stream, true, fmt.Sprintf("keyspace %s created (%s)", name, want.summary()), keyspaceOutput(name, want, durable, false))
		}
		return m.alterKeyspace(ctx, stream, s, name, password, want, durable, live)

	case convergeMatches:
		// The no-op path WAITS too, and this is the case the wait exists for. The
		// three-way read finishes a missing statement; it cannot finish a missing
		// AGREEMENT. A previous run killed between its CREATE and its wait leaves
		// exactly this state — present on the coordinator this run reads, absent on a
		// lagging replica — and reporting success without waiting would hand the next
		// step the failure the whole mechanism is meant to prevent. When agreement
		// already holds this costs one round trip and returns.
		if err := awaitSchema(ctx, s, "keyspace "+name); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		return sendOutcome(stream, false, fmt.Sprintf("keyspace %s already matches (no-op)", name), keyspaceOutput(name, want, durable, false))

	default:
		return m.alterKeyspace(ctx, stream, s, name, password, want, durable, live)
	}
}

// alterKeyspace moves an EXISTING keyspace to the declared replication. It is a
// function rather than a switch arm because two paths reach it: the plain
// `convergeDiffers`, and the `convergeAbsent` path whose CREATE turned out to be a
// server-side no-op against a keyspace this coordinator had not seen. Both owe the
// same repair decision, computed against what is actually there.
func (m *CassandraModule) alterKeyspace(ctx context.Context, stream eventStream, s cassSession,
	name, password string, want replicationSpec, durable bool, live keyspaceState) error {

	repair := want.raisesOver(live.replication)
	stmt := fmt.Sprintf("ALTER KEYSPACE %s WITH REPLICATION = %s AND DURABLE_WRITES = %t", name, want.literal(), durable)
	if err := s.Exec(ctx, stmt); err != nil {
		return sendFailure(stream, "ALTER KEYSPACE: "+redactError(err, password))
	}
	if err := awaitSchema(ctx, s, "keyspace "+name); err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	message := fmt.Sprintf("keyspace %s altered: %s -> %s", name, live.replication.summary(), want.summary())
	if repair {
		// Said in the message as well as in Output, because this is the one outcome
		// of this state that is NOT finished when the state reports success, and a
		// scenario author reading a run log should not have to know to look at a
		// field.
		message += " — REPAIR REQUIRED: the new replicas hold no data until `nodetool repair` runs on the affected datacenters; this state does not run it"
	}
	return sendOutcome(stream, true, message, keyspaceOutput(name, want, durable, repair))
}

// keyspaceConverge is the three-way read of schema.go, named so the branch an
// outcome came from is the same word the design uses for it.
func keyspaceConverge(found bool, live keyspaceState, want replicationSpec, durable bool) converge {
	switch {
	case !found:
		return convergeAbsent
	case want.equal(live.replication) && durable == live.durableWrites:
		return convergeMatches
	default:
		return convergeDiffers
	}
}

// keyspaceOutput is the one shape every outcome of `present` reports, so a scenario
// reading register.self.* finds the same fields whichever branch ran.
func keyspaceOutput(name string, repl replicationSpec, durable, repair bool) map[string]any {
	return map[string]any{
		"keyspace":        name,
		"replication":     repl.summary(),
		"durable_writes":  durable,
		"repair_required": repair,
	}
}

// applyKeyspaceAbsent drops a keyspace. Idempotent: one that is not there is a
// no-op.
func (m *CassandraModule) applyKeyspaceAbsent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	name := stringOrEmpty(f["name"])
	if err := checkIdentifier("params.name", name); err != nil {
		return sendFailure(stream, err.Error())
	}

	if _, found, err := readKeyspace(ctx, s, name); err != nil {
		return sendFailure(stream, redactError(err, password))
	} else if !found {
		// Waits for the same reason `present` does on its no-op: a previous run may
		// have dropped it and died before the cluster agreed.
		if err := awaitSchema(ctx, s, "keyspace "+name); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		return sendOutcome(stream, false, fmt.Sprintf("keyspace %s is already absent (no-op)", name), map[string]any{"keyspace": name})
	}

	// IF EXISTS for the mirror of the CREATE case: a coordinator that has not yet
	// received someone else's DROP would otherwise turn this into a hard failure.
	if err := s.Exec(ctx, "DROP KEYSPACE IF EXISTS "+name); err != nil {
		return sendFailure(stream, "DROP KEYSPACE: "+redactError(err, password))
	}
	if err := awaitSchema(ctx, s, "keyspace "+name); err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	return sendOutcome(stream, true, fmt.Sprintf("keyspace %s dropped", name), map[string]any{"keyspace": name})
}

// validateKeyspacePresent refuses before the run everything [applyKeyspacePresent]
// would refuse during it (NIM-786): the point of the phase is to say no BEFORE
// anything happened, and what it lets through silently makes the phase worthless.
func validateKeyspacePresent(f map[string]*structpb.Value) []string {
	errs := validateConnect(f)
	if err := checkIdentifier("params.name", stringOrEmpty(f["name"])); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := parseReplication(f["replication"]); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

func validateKeyspaceAbsent(f map[string]*structpb.Value) []string {
	errs := validateConnect(f)
	if err := checkIdentifier("params.name", stringOrEmpty(f["name"])); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}
