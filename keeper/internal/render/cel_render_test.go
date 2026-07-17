package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// stubKVRender — hermetic KVReader for render tests of vault() in CEL params.
type stubKVRender struct {
	secrets map[string]map[string]any
}

func (s stubKVRender) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if d, ok := s.secrets[path]; ok {
		return d, nil
	}
	return nil, context.Canceled // any not-found error; test covers the success path
}

// vault() in CEL params resolves keeper-side: the actual secret value lands
// in Params (not a ref string). Engine built with a KVReader (WithVault).
func TestRenderParams_VaultKeeperSide(t *testing.T) {
	kv := stubKVRender{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	params := map[string]any{
		"cmd":   "redis-cli AUTH ${ vault('secret/redis/admin').password }",
		"token": "${ vault('secret/redis/admin#password') }",
	}
	vars := cel.Vars{Ctx: context.Background()}

	st, err := renderParams(e, params, vars)
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	f := st.GetFields()
	if got := f["cmd"].GetStringValue(); got != "redis-cli AUTH s3cr3t" {
		t.Errorf("command = %q, want the real secret value in Params", got)
	}
	if got := f["token"].GetStringValue(); got != "s3cr3t" {
		t.Errorf("token = %q, want s3cr3t (#field)", got)
	}
}

// TestRenderParams_NestedAndPassthrough — interpolation in nested map/list;
// non-string values pass through untouched.
func TestRenderParams_NestedAndPassthrough(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	params := map[string]any{
		"cmd":     "echo ${ input.user }",
		"flags":   []any{"--name", "${ input.user }"},
		"opts":    map[string]any{"timeout": int64(30), "label": "${ input.user }-svc"},
		"enabled": true,
	}
	vars := cel.Vars{Input: map[string]any{"user": "bob"}}

	st, err := renderParams(e, params, vars)
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	f := st.GetFields()
	if got := f["cmd"].GetStringValue(); got != "echo bob" {
		t.Errorf("command = %q", got)
	}
	if got := f["flags"].GetListValue().GetValues()[1].GetStringValue(); got != "bob" {
		t.Errorf("flags[1] = %q", got)
	}
	if got := f["opts"].GetStructValue().GetFields()["label"].GetStringValue(); got != "bob-svc" {
		t.Errorf("opts.label = %q", got)
	}
	if got := f["opts"].GetStructValue().GetFields()["timeout"].GetNumberValue(); got != 30 {
		t.Errorf("opts.timeout = %v", got)
	}
	if got := f["enabled"].GetBoolValue(); !got {
		t.Errorf("enabled = %v", got)
	}
}

// TestRenderParams_NativeTypeSingleBlock — a bare ${expr} with no wrapping
// yields a native type (number), not a string (templating.md §5(a)).
func TestRenderParams_NativeTypeSingleBlock(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	params := map[string]any{"n": "${ input.count }"}
	vars := cel.Vars{Input: map[string]any{"count": int64(5)}}

	st, err := renderParams(e, params, vars)
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	if got := st.GetFields()["n"].GetNumberValue(); got != 5 {
		t.Errorf("n = %v, want native number 5", got)
	}
}

// TestEvalWhere_EmptyTrue — empty where: → true (no filter).
func TestEvalWhere_EmptyTrue(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	ok, err := evalWhere(e, "", cel.Vars{})
	if err != nil || !ok {
		t.Fatalf("evalWhere(\"\") = %v, %v; want true, nil", ok, err)
	}
}

// TestEvalWhere_SoulprintSelf — where: reads a host's soulprint.self.*.
func TestEvalWhere_SoulprintSelf(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	vars := cel.Vars{SoulprintSelf: map[string]any{"os": map[string]any{"family": "debian"}}}
	ok, err := evalWhere(e, "soulprint.self.os.family == 'debian'", vars)
	if err != nil {
		t.Fatalf("evalWhere: %v", err)
	}
	if !ok {
		t.Error("evalWhere = false, want true for debian host")
	}
}

// TestEvalWhere_SoulprintSelfChoirs — where: filters by a host's choir
// membership (ADR-044, S-T4): `X in soulprint.self.choirs`.
func TestEvalWhere_SoulprintSelfChoirs(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	member := soulprintSelfMap(&topology.HostFacts{
		SID:    "a.example.com",
		Choirs: []string{"primaries", "voters"},
	})
	ok, err := evalWhere(e, "'voters' in soulprint.self.choirs", cel.Vars{SoulprintSelf: member})
	if err != nil {
		t.Fatalf("evalWhere (member): %v", err)
	}
	if !ok {
		t.Error("evalWhere = false for choir 'voters' member, want true")
	}

	outsider := soulprintSelfMap(&topology.HostFacts{SID: "b.example.com"})
	ok, err = evalWhere(e, "'voters' in soulprint.self.choirs", cel.Vars{SoulprintSelf: outsider})
	if err != nil {
		t.Fatalf("evalWhere (outsider): %v", err)
	}
	if ok {
		t.Error("evalWhere = true for a host with no choir membership, want false")
	}
}

// TestEvalWhere_SoulprintSelfTraits — GUARD (ADR-060): where: targets by a
// host's operator-set traits (registry projection soulprint.self.traits).
// Scalar `traits.namespace == 'dba-ns'` and list `'alice' in traits.owners`
// both resolve WITHOUT AST rewrite (traits is a plain map field under self).
// Covers a match and a non-match.
func TestEvalWhere_SoulprintSelfTraits(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	dbaHost := soulprintSelfMap(&topology.HostFacts{
		SID: "db-1.example.com",
		Traits: map[string]any{
			"namespace": "dba-ns",
			"owners":    []any{"alice", "bob"},
			"product":   "aboba",
		},
	})
	otherHost := soulprintSelfMap(&topology.HostFacts{
		SID: "web-1.example.com",
		Traits: map[string]any{
			"namespace": "web-ns",
			"owners":    []any{"carol"},
		},
	})
	// A host with no traits at all (nil map → empty object under
	// self.traits): a key lookup is a plain no-such-key, predicate → false
	// (not a panic/error).
	noTraitsHost := soulprintSelfMap(&topology.HostFacts{SID: "bare.example.com"})

	cases := []struct {
		name string
		expr string
		self map[string]any
		want bool
	}{
		{"scalar match", "soulprint.self.traits.namespace == 'dba-ns'", dbaHost, true},
		{"scalar no-match", "soulprint.self.traits.namespace == 'dba-ns'", otherHost, false},
		{"list contains match", "'alice' in soulprint.self.traits.owners", dbaHost, true},
		{"list contains no-match", "'alice' in soulprint.self.traits.owners", otherHost, false},
		{"missing key on no-traits host", "has(soulprint.self.traits.namespace) && soulprint.self.traits.namespace == 'dba-ns'", noTraitsHost, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalWhere(e, tc.expr, cel.Vars{SoulprintSelf: tc.self})
			if err != nil {
				t.Fatalf("evalWhere(%q): %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("evalWhere(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestSoulprintSelfMap_Traits — GUARD (ADR-060): soulprint.self.traits is
// always present (empty map when nil) and carries operator-set values as-is
// (scalar + list). Symmetry with hostFactsToMap is checked below.
func TestSoulprintSelfMap_Traits(t *testing.T) {
	h := &topology.HostFacts{
		SID: "db-1.example.com",
		Traits: map[string]any{
			"namespace": "dba-ns",
			"owners":    []any{"alice", "bob"},
		},
	}
	self := soulprintSelfMap(h)
	traits, ok := self["traits"].(map[string]any)
	if !ok {
		t.Fatalf("self.traits type = %T, want map[string]any", self["traits"])
	}
	if traits["namespace"] != "dba-ns" {
		t.Errorf("self.traits.namespace = %v, want dba-ns", traits["namespace"])
	}
	owners, _ := traits["owners"].([]any)
	if len(owners) != 2 || owners[0] != "alice" || owners[1] != "bob" {
		t.Errorf("self.traits.owners = %v, want [alice bob]", traits["owners"])
	}

	// nil Traits → empty map (not a missing traits key under self).
	bare := soulprintSelfMap(&topology.HostFacts{SID: "bare.example.com"})
	if m, ok := bare["traits"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("self.traits for nil-traits host = %v, want empty map", bare["traits"])
	}

	// Symmetry: a soulprint.hosts element carries the same traits.
	elem := hostFactsToMap(h)
	htraits, ok := elem["traits"].(map[string]any)
	if !ok || htraits["namespace"] != "dba-ns" {
		t.Errorf("hosts[].traits = %v, want consistent with self", elem["traits"])
	}
}

// TestSoulprintSelfMap_TraitsRegistryWinsOverReported — GUARD (ADR-060):
// anti-spoofing. Operator-set traits live in the registry (HostFacts.Traits);
// if a Soul agent reports soulprint with a "traits" key, the registry
// projection MUST override it — Soul can't spoof operator traits (targeting
// by them is a trusted operator decision). Mirrors the sid/covens invariant
// (TestSoulprintSelfMap_RegistryWinsOverReported).
func TestSoulprintSelfMap_TraitsRegistryWinsOverReported(t *testing.T) {
	h := &topology.HostFacts{
		SID:    "db-1.example.com",
		Traits: map[string]any{"namespace": "dba-ns"},
		Soulprint: map[string]any{
			"traits": map[string]any{"namespace": "spoofed"},
		},
	}
	self := soulprintSelfMap(h)

	traits, ok := self["traits"].(map[string]any)
	if !ok {
		t.Fatalf("self.traits type = %T, want map[string]any", self["traits"])
	}
	if traits["namespace"] != "dba-ns" {
		t.Errorf("self.traits.namespace = %v, registry must override reported (anti-spoofing)", traits["namespace"])
	}
}

// TestSoulprintSelfMap_NullReportedFacts — BUG E2E #3: with NULL reported
// soulprint (Soul agent hasn't reported), soulprint.self.sid/.covens/.role
// are still available from the roster (registry projection, ADR-018).
func TestSoulprintSelfMap_NullReportedFacts(t *testing.T) {
	h := &topology.HostFacts{
		SID:       "web-2.example.com",
		Coven:     []string{"prod", "web"},
		Role:      "replica",
		Soulprint: nil, // Soul hasn't sent a SoulprintReport yet
	}
	self := soulprintSelfMap(h)

	if got := self["sid"]; got != "web-2.example.com" {
		t.Errorf("self.sid = %v, want roster SID", got)
	}
	covens, ok := self["covens"].([]any)
	if !ok || len(covens) != 2 || covens[0] != "prod" || covens[1] != "web" {
		t.Errorf("self.covens = %v, want roster coven [prod web]", self["covens"])
	}
	if got := self["role"]; got != "replica" {
		t.Errorf("self.role = %v, want roster role", got)
	}
}

// TestSoulprintSelfMap_MergeReported — reported os/network are available
// alongside registry sid/covens/role.
func TestSoulprintSelfMap_MergeReported(t *testing.T) {
	h := &topology.HostFacts{
		SID:   "web-1.example.com",
		Coven: []string{"prod"},
		Role:  "primary",
		Soulprint: map[string]any{
			"os":      map[string]any{"family": "debian"},
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
	}
	self := soulprintSelfMap(h)

	osSec, _ := self["os"].(map[string]any)
	if osSec == nil || osSec["family"] != "debian" {
		t.Errorf("self.os.family lost after merge: %v", self["os"])
	}
	netSec, _ := self["network"].(map[string]any)
	if netSec == nil || netSec["primary_ip"] != "10.0.0.1" {
		t.Errorf("self.network.primary_ip lost after merge: %v", self["network"])
	}
	if self["sid"] != "web-1.example.com" {
		t.Errorf("self.sid = %v, want roster SID", self["sid"])
	}
}

// TestSoulprintSelfMap_RegistryWinsOverReported — if sid/covens sneaks into
// the reported map, the roster wins (registry is the source of truth, ADR-018).
func TestSoulprintSelfMap_RegistryWinsOverReported(t *testing.T) {
	h := &topology.HostFacts{
		SID:   "authoritative.example.com",
		Coven: []string{"prod"},
		Soulprint: map[string]any{
			"sid":    "spoofed.example.com",
			"covens": []any{"attacker"},
		},
	}
	self := soulprintSelfMap(h)

	if self["sid"] != "authoritative.example.com" {
		t.Errorf("self.sid = %v, registry must override reported", self["sid"])
	}
	covens, _ := self["covens"].([]any)
	if len(covens) != 1 || covens[0] != "prod" {
		t.Errorf("self.covens = %v, registry must override reported", self["covens"])
	}
}

// TestSoulprintSelfMap_NoMutateRoster — soulprintSelfMap doesn't corrupt the
// roster's host.Soulprint (top-level deep copy).
func TestSoulprintSelfMap_NoMutateRoster(t *testing.T) {
	reported := map[string]any{"os": map[string]any{"family": "debian"}}
	h := &topology.HostFacts{SID: "h.example.com", Coven: []string{"prod"}, Soulprint: reported}

	_ = soulprintSelfMap(h)

	if _, leaked := reported["sid"]; leaked {
		t.Error("soulprintSelfMap mutated host.Soulprint (added sid)")
	}
	if _, leaked := reported["covens"]; leaked {
		t.Error("soulprintSelfMap mutated host.Soulprint (added covens)")
	}
}

// TestEvalWhere_AddReplicasNullFacts — regression E2E #3: add_replicas'
// where: renders without "no such key: sid" under NULL reported facts.
func TestEvalWhere_AddReplicasNullFacts(t *testing.T) {
	e, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	in := RenderInput{
		Input: map[string]any{"replicas": []any{"web-1.example.com"}},
	}
	h := &topology.HostFacts{SID: "web-2.example.com", Coven: []string{"prod"}, Soulprint: nil}
	vars := hostVars(in, h, 1)

	ok, err := evalWhere(e, "!(soulprint.self.sid in input.replicas)", vars)
	if err != nil {
		t.Fatalf("evalWhere add_replicas: %v", err)
	}
	if !ok {
		t.Error("evalWhere = false, want true (web-2 not in replicas)")
	}
}

// TestSoulprintSelf_HostsSymmetry — self and a soulprint.hosts element agree
// on sid/covens/role/choirs.
func TestSoulprintSelf_HostsSymmetry(t *testing.T) {
	h := &topology.HostFacts{
		SID:    "web-1.example.com",
		Coven:  []string{"prod", "web"},
		Role:   "primary",
		Choirs: []string{"primaries", "voters"},
	}
	self := soulprintSelfMap(h)
	hostsEl := hostFactsToMap(h)

	if self["sid"] != hostsEl["sid"] {
		t.Errorf("sid mismatch: self=%v hosts=%v", self["sid"], hostsEl["sid"])
	}
	if self["role"] != hostsEl["role"] {
		t.Errorf("role mismatch: self=%v hosts=%v", self["role"], hostsEl["role"])
	}
	assertListSymmetry(t, "covens", self["covens"], hostsEl["covens"])
	assertListSymmetry(t, "choirs", self["choirs"], hostsEl["choirs"])
}

func assertListSymmetry(t *testing.T, name string, selfV, hostsV any) {
	t.Helper()
	selfL, _ := selfV.([]any)
	hostsL, _ := hostsV.([]any)
	if len(selfL) != len(hostsL) {
		t.Fatalf("%s len mismatch: self=%v hosts=%v", name, selfL, hostsL)
	}
	for i := range selfL {
		if selfL[i] != hostsL[i] {
			t.Errorf("%s[%d] mismatch: self=%v hosts=%v", name, i, selfL[i], hostsL[i])
		}
	}
}
