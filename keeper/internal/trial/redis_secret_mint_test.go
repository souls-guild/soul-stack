package trial

// Guards on the DECLARED-SECRET axis of the redis scenarios ([ADR-0083]). They replace the
// kv-present guards of NIM-172: a scenario no longer writes a Vault path, so "the generate
// step is present / read-set ⊆ generated-set" has no subject left. What survived, restated:
//
//  1. PRESENCE + ORDER — every field consumed as `register.<field>` is produced by a
//     keeper-side `core.state.set` task in the SAME plan, in a strictly earlier Passage.
//     Under NIM-172 that order rested on the vault-emitter axis, invisible to the scenario
//     author; now it is a plain register edge (shared/config.collectTaskReads), declared by
//     the scenario itself.
//  2. SCOPE — the mint requests a NEW secret for exactly the accounts the run is meant to
//     create and for no other. Requesting one for an already-deployed account hands the live
//     instance a credential nothing else knows (community.redis.acl AUTHs as default_admin
//     BEFORE ACL LOAD applies the new file) and silently rotates a working client's password.
//     Checked on the RENDERED plan via the `__secret_request` envelope, not on source text.
//  3. COVERAGE — every account the plan looks up BY LITERAL NAME out of a resolved register
//     is in that field's minted set. Successor of the read-set ⊆ generated-set drift guard: a
//     deploy body that starts authenticating as a system account whose name never reaches the
//     mint list fails here.
//
// Why Go and not case.yml: L0 renders but never EXECUTES the keeper-side mint — the register
// mock stands in for its output, so an L0 case is green whatever the mint produces. And
// case.yml cannot express Passage order at all.
//
// The complementary property — the value the mint registers is a `vault:` ref, never
// plaintext — is pinned keeper-side (render.TestResolveRegisterSecrets_*) and by the seal
// tests; it is deliberately not re-checked here.

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/secretpolicy"
)

const (
	createSentinelCase    = "../../../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml"
	createClusterCase     = "../../../examples/service/redis/scenario/create/tests/cluster-create-3shards/case.yml"
	createSentinelAclCase = "../../../examples/service/redis/scenario/create/tests/sentinel-acl-users/case.yml"
	fromSoulsClusterCase  = "../../../examples/service/redis/scenario/create_from_souls/tests/cluster-from-souls-3shards/case.yml"
	fromSoulsSentinelCase = "../../../examples/service/redis/scenario/create_from_souls/tests/sentinel-from-souls-1master-2replica/case.yml"
	addUserSystemCase     = "../../../examples/service/redis/scenario/add_user/tests/add-user-preserves-system-users/case.yml"
	updateUsersSystemCase = "../../../examples/service/redis/scenario/update_users/tests/preserves-system-users/case.yml"

	// stateSetAddr — the keeper-side module that reads-or-mints a state field carrying
	// declared secrets ([ADR-0083] §4). It is the ONLY channel into the service's own Vault
	// namespace, so "is there a mint for X" is exactly "is there a core.state.set for X".
	stateSetAddr = "core.state.set"

	systemUsersField = "system_acl_users"
	redisUsersField  = "redis_users"
)

// loadExpandedPlan loads the scenario a case belongs to, expands its includes and stratifies
// the result — the same three steps prod runs before dispatch.
func loadExpandedPlan(t *testing.T, caseFile string) ([]config.Task, config.Passage) {
	t.Helper()
	scnPath := scenarioPathFor(caseFile)
	scn, _, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest(%s): %v", scnPath, err)
	}
	if hasErrors(diags) {
		t.Fatalf("scenario %s invalid: %s", scnPath, formatDiags(diags))
	}
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if hasErrors(iDiags) {
		t.Fatalf("expand includes: %s", formatDiags(iDiags))
	}
	passage, perr := config.Stratify(expanded)
	if perr != nil {
		t.Fatalf("Stratify: %v", perr)
	}
	return expanded, passage
}

// mintIndexByRegister maps each mint task's register name to its index. A field minted twice
// in one plan is fatal: the consumers' edge would be ambiguous.
func mintIndexByRegister(t *testing.T, tasks []config.Task) map[string]int {
	t.Helper()
	out := map[string]int{}
	for i := range tasks {
		if tasks[i].Module == nil || tasks[i].Module.Module != stateSetAddr || tasks[i].Register == "" {
			continue
		}
		if prev, dup := out[tasks[i].Register]; dup {
			t.Fatalf("register %q is produced by two core.state.set tasks (idx %d and %d) — the consumers' edge is ambiguous", tasks[i].Register, prev, i)
		}
		out[tasks[i].Register] = i
	}
	return out
}

// passageReadText flattens exactly the surfaces shared/config.collectTaskReads walks —
// where, loop.when, vars, output, loop.items, module params, apply input, block children.
// One traversal serves both "who reads register X" and the literal-lookup scan, and it is the
// PASSAGE-DEFINING set: a register named only in `when:` is a read but not an edge, and must
// not be asserted on.
func passageReadText(t *config.Task) string {
	var sb strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			sb.WriteString(x)
			sb.WriteByte('\n')
		case map[string]any:
			for _, sub := range x {
				walk(sub)
			}
		case []any:
			for _, sub := range x {
				walk(sub)
			}
		}
	}
	sb.WriteString(t.Where)
	sb.WriteByte('\n')
	if t.Loop != nil {
		sb.WriteString(t.Loop.When)
		sb.WriteByte('\n')
		walk(t.Loop.Items)
	}
	walk(t.Vars)
	walk(t.Output)
	if t.Module != nil {
		walk(t.Module.Params)
	}
	if t.Apply != nil {
		walk(t.Apply.Input)
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			sb.WriteString(passageReadText(&t.Block.Block[i]))
		}
	}
	return sb.String()
}

// registerReaders returns the indices of tasks holding a passage-defining reference to
// register `name` — the same edges config.Stratify builds from.
func registerReaders(tasks []config.Task, name string) []int {
	var out []int
	for i := range tasks {
		for _, ref := range config.ExtractRegisterRefs(passageReadText(&tasks[i])) {
			if ref == name {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// assertMintPrecedesReaders is invariant 1 for one scenario.
func assertMintPrecedesReaders(t *testing.T, caseFile string) {
	t.Helper()
	tasks, passage := loadExpandedPlan(t, caseFile)
	mints := mintIndexByRegister(t, tasks)
	if len(mints) == 0 {
		t.Fatalf("%s: no core.state.set task in the plan — nothing resolves the scenario's declared secrets ([ADR-0083] §4)", caseFile)
	}

	consumers := 0
	for reg, mi := range mints {
		for _, ri := range registerReaders(tasks, reg) {
			if ri == mi {
				continue
			}
			consumers++
			if rp, mp := passage.TaskPassage[ri], passage.TaskPassage[mi]; rp <= mp {
				t.Fatalf("%s: task %q reads register.%s in passage %d, the mint sits in passage %d — the mint MUST be strictly earlier, else the consumer renders against an unresolved register",
					caseFile, tasks[ri].Name, reg, rp, mp)
			}
		}
	}
	if consumers == 0 {
		t.Fatalf("%s: nothing reads a minted register — the guard lost its subject (did the consumers stop reading register.<field>?)", caseFile)
	}
}

// TestRedisSecrets_MintPrecedesRegisterReads — invariant 1 across every redis scenario that
// mints or resolves a declared secret. create carries a provision body (a second, roster
// axis); create_from_souls, add_user and update_users have none, so there the register edge
// is the only thing that could be holding the order.
func TestRedisSecrets_MintPrecedesRegisterReads(t *testing.T) {
	for _, c := range []string{
		createSentinelCase,
		createClusterCase,
		fromSoulsClusterCase,
		fromSoulsSentinelCase,
		addUserSystemCase,
		updateUsersSystemCase,
	} {
		t.Run(scenarioLabel(c), func(t *testing.T) { assertMintPrecedesReaders(t, c) })
	}
}

// scenarioLabel is the scenario directory name of a case path.
func scenarioLabel(caseFile string) string {
	parts := strings.Split(caseFile, "/")
	for i, p := range parts {
		if p == "scenario" && i+1 < len(parts) {
			return parts[i+1] + "/" + parts[len(parts)-2]
		}
	}
	return caseFile
}

// TestRedisSecrets_RegisterEdgeIsLoadBearing — the mutation proof for invariant 1, run on
// create, the one scenario carrying a second axis. Two halves:
//
//	A. the provision body (the plan's only roster refresh emitter) is filtered out and the
//	   order STILL holds — so it is not the roster axis carrying it;
//	B. the mint's `register:` is then cleared, and the plan must stop stratifying at all.
//
// B is what makes A meaningful: if the edge were decorative, deleting it would leave a plan
// that still stratifies, and the guard above would stay green after the order broke.
func TestRedisSecrets_RegisterEdgeIsLoadBearing(t *testing.T) {
	tasks, _ := loadExpandedPlan(t, createSentinelCase)

	// Two passes: the second drops the captures of what the first dropped.
	// [ADR-0084] turned `provisioned_vm_ids` from an end-of-run block into a
	// `core.state.set` task reading `register.provision`, so a capture is now part
	// of the provision body — leaving it behind makes Stratify reject the plan on a
	// dangling register instead of answering the A/B question.
	dropped := map[string]bool{}
	isProvisionBody := func(t config.Task) bool {
		addr := ""
		if t.Module != nil {
			addr = t.Module.Module
		}
		return strings.HasPrefix(addr, "core.cloud") ||
			strings.HasPrefix(addr, "core.bootstrap") ||
			strings.HasPrefix(addr, "core.soul")
	}
	for i := range tasks {
		if isProvisionBody(tasks[i]) && tasks[i].Register != "" {
			dropped[tasks[i].Register] = true
		}
	}
	var filtered []config.Task
	for i := range tasks {
		if isProvisionBody(tasks[i]) {
			continue
		}
		readsDropped := false
		for _, ref := range config.ExtractRegisterRefs(passageReadText(&tasks[i])) {
			if dropped[ref] {
				readsDropped = true
				break
			}
		}
		if readsDropped {
			continue
		}
		filtered = append(filtered, tasks[i])
	}

	held, err := config.Stratify(filtered)
	if err != nil {
		t.Fatalf("Stratify (without the provision body): %v", err)
	}
	mints := mintIndexByRegister(t, filtered)
	mi, ok := mints[systemUsersField]
	if !ok {
		t.Fatalf("the mint of %s disappeared with the provision body — the filter is too broad", systemUsersField)
	}
	readers := registerReaders(filtered, systemUsersField)
	if len(readers) < 2 {
		t.Fatalf("only %d task(s) reference register.%s — the A/B has no subject", len(readers), systemUsersField)
	}
	for _, ri := range readers {
		if ri != mi && held.TaskPassage[ri] <= held.TaskPassage[mi] {
			t.Fatalf("with the roster axis removed, %q already shares the mint's passage — the register axis is not what holds this order", filtered[ri].Name)
		}
	}

	broken := make([]config.Task, len(filtered))
	copy(broken, filtered)
	broken[mi].Register = ""
	if _, err := config.Stratify(broken); err == nil {
		t.Fatalf("with the mint's register removed the plan still stratifies — the consumers are ordered by something other than this edge, so the guard above cannot detect a broken edge")
	} else if !strings.Contains(err.Error(), systemUsersField) {
		t.Fatalf("Stratify rejected the mutated plan for an unrelated reason: %v", err)
	}
}

// mintedSet is the rendered `value` of one core.state.set task: account name → does this run
// request a NEW secret for it. The request travels as the `__secret_request` envelope
// (shared/secretpolicy.MarkerKey), so "was a secret requested" is decidable on rendered params.
func mintedSet(t *testing.T, caseFile string, tasks []*render.RenderedTask, field string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	found := false
	for _, rt := range tasks {
		if rt.Module != stateSetAddr || rt.Params == nil {
			continue
		}
		params := rt.Params.AsMap()
		if name, _ := params["field"].(string); name != field {
			continue
		}
		if found {
			t.Fatalf("%s: two rendered core.state.set tasks for field %q", caseFile, field)
		}
		found = true
		value, ok := params["value"].([]any)
		if !ok {
			t.Fatalf("%s: core.state.set[%s].value is %T, want a list", caseFile, field, params["value"])
		}
		for i, item := range value {
			obj, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("%s: core.state.set[%s].value[%d] is %T, want an object", caseFile, field, i, item)
			}
			name, _ := obj["name"].(string)
			if name == "" {
				t.Fatalf("%s: core.state.set[%s].value[%d] carries no name: %v", caseFile, field, i, obj)
			}
			out[name] = isSecretRequest(obj["password"])
		}
	}
	if !found {
		t.Fatalf("%s: no rendered core.state.set task for field %q — nothing resolves that field's declared secrets", caseFile, field)
	}
	return out
}

func isSecretRequest(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m[secretpolicy.MarkerKey]
	return ok
}

// renderPlan renders one L0 case through the shared harness path.
func renderPlan(t *testing.T, caseFile string) []*render.RenderedTask {
	t.Helper()
	c, file, err := LoadCase(caseFile)
	if err != nil {
		t.Fatalf("LoadCase(%s): %v", caseFile, err)
	}
	rc, err := renderCase(context.Background(), c, file)
	if err != nil {
		t.Fatalf("render %s: %v", caseFile, err)
	}
	return rc.tasks
}

// TestRedisAddUser_MintsOnlyTheNewUser — invariant 2 for add_user: a secret is requested for
// input.user and for nobody else. Mutation: drop the ternary in the mint's `value` (request for
// every element) and this fails on bob and on every system account.
func TestRedisAddUser_MintsOnlyTheNewUser(t *testing.T) {
	tasks := renderPlan(t, addUserSystemCase)

	operator := mintedSet(t, addUserSystemCase, tasks, redisUsersField)
	if len(operator) < 2 {
		t.Fatalf("the operator set holds %d element(s) — the fixture no longer carries a pre-existing user, so the assertions below are vacuous", len(operator))
	}
	if !operator["alice"] {
		t.Errorf("add_user requests no secret for the new user alice; requested: %v", requestedNames(operator))
	}
	if operator["bob"] {
		t.Errorf("add_user requests a NEW secret for the already-deployed user bob — a re-run would rotate a credential its clients still hold")
	}

	system := mintedSet(t, addUserSystemCase, tasks, systemUsersField)
	if len(system) == 0 {
		t.Fatalf("the system set is empty — add_user resolves no service account at all")
	}
	for _, name := range requestedNames(system) {
		t.Errorf("add_user requests a NEW secret for the service's own account %q — the live instance authenticates with it (community.redis.acl AUTHs as default_admin BEFORE ACL LOAD)", name)
	}
}

// TestRedisUpdateUsers_MintsOnlyTheOperatorSet — invariant 2 for update_users: the
// bulk-replace requests secrets for the incoming set and never for a service account.
func TestRedisUpdateUsers_MintsOnlyTheOperatorSet(t *testing.T) {
	tasks := renderPlan(t, updateUsersSystemCase)

	operator := mintedSet(t, updateUsersSystemCase, tasks, redisUsersField)
	for _, want := range []string{"alice", "carol"} {
		if !operator[want] {
			t.Errorf("update_users requests no secret for %q in the incoming set; requested: %v", want, requestedNames(operator))
		}
	}

	system := mintedSet(t, updateUsersSystemCase, tasks, systemUsersField)
	if len(system) == 0 {
		t.Fatalf("the system set is empty — the guard below is vacuous")
	}
	for _, name := range requestedNames(system) {
		t.Errorf("update_users requests a NEW secret for the service's own account %q — a bulk-replace must not rotate the credentials the live instance runs on", name)
	}
}

// requestedNames — the accounts of a minted set carrying a generate request, sorted.
func requestedNames(set map[string]bool) []string {
	var out []string
	for name, req := range set {
		if req {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// literalLookup matches `register.<field>.effective … )['<name>']` — a task authenticating as
// ONE named account out of a resolved register.
//
// The account name is `[^'"]+`, not a character class of what today's names happen to use:
// this pattern is the guard's SUBJECT FINDER, so anything it fails to match is silently
// not checked. A `['sentinel-1']` or a `["haproxy"]` under a narrower pattern would not
// read as a violation — it would read as no lookup at all. Both quote styles for the same
// reason; CEL accepts either.
var literalLookup = regexp.MustCompile(`register\.([A-Za-z0-9_]+)\.effective[^"']*?\)\[['"]([^'"]+)['"]\]`)

// TestRedisCreate_MintCoversEveryLiteralAccountLookup — invariant 3. Every account the create
// plan looks up by literal name must be in that field's minted set; otherwise the run mints
// one set of credentials and authenticates with another.
//
// It walks the EXPANDED task list, which is wider than what renderPlan reaches: a lookup on
// a task the case's hosts do render is caught by render itself (the missing key errors), so
// what is left to this guard is the tasks a run of this shape never renders. The pattern is
// the subject finder — see TestLiteralLookup_Spellings for its own guard.
func TestRedisCreate_MintCoversEveryLiteralAccountLookup(t *testing.T) {
	tasks, _ := loadExpandedPlan(t, createSentinelAclCase)
	rendered := renderPlan(t, createSentinelAclCase)

	wanted := map[string]map[string]bool{} // field → names looked up literally
	for i := range tasks {
		for _, m := range literalLookup.FindAllStringSubmatch(passageReadText(&tasks[i]), -1) {
			if wanted[m[1]] == nil {
				wanted[m[1]] = map[string]bool{}
			}
			wanted[m[1]][m[2]] = true
		}
	}
	if len(wanted) == 0 {
		t.Fatalf("no literal account lookup found in the create plan — the guard lost its subject")
	}

	for field, names := range wanted {
		minted := mintedSet(t, createSentinelAclCase, rendered, field)
		for name := range names {
			if _, ok := minted[name]; !ok {
				t.Errorf("the plan authenticates as %q out of register.%s, but the mint set is %v — that account would have no credential on a fresh incarnation", name, field, sortedNames(minted))
			}
		}
	}
}

// TestLiteralLookup_Spellings guards the SUBJECT FINDER of the test above. A lookup the
// pattern fails to match does not read as a violation — it reads as no lookup at all, and
// the guard silently checks one account fewer. Account names are not constrained to the
// spelling today's happen to use, and CEL accepts either quote style.
func TestLiteralLookup_Spellings(t *testing.T) {
	const prefix = "${ merge(register.system_acl_users.effective.map(u, { u.name: u.password }))"
	cases := []struct {
		expr, name string // name == "" → must not match
	}{
		{prefix + "['default_admin'] }", "default_admin"},
		{prefix + `["haproxy"] }`, "haproxy"},
		{prefix + "['sentinel-1'] }", "sentinel-1"},
		{prefix + `["ha.proxy@1"] }`, "ha.proxy@1"},
		// Not a literal: a computed key is the plan reading its own covenant, which is
		// exactly the construction this whole invariant asks authors to prefer.
		{prefix + "[compute.admin_user] }", ""},
	}
	for _, tc := range cases {
		m := literalLookup.FindStringSubmatch(tc.expr)
		if tc.name == "" {
			if m != nil {
				t.Errorf("%s\n  matched %q, want no match", tc.expr, m[2])
			}
			continue
		}
		if m == nil {
			t.Errorf("%s\n  no match — the account would be silently unchecked", tc.expr)
			continue
		}
		if m[1] != "system_acl_users" || m[2] != tc.name {
			t.Errorf("%s\n  = (%q, %q), want (%q, %q)", tc.expr, m[1], m[2], "system_acl_users", tc.name)
		}
	}
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
