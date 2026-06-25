package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// stubKVRender — герметичный KVReader для render-тестов vault() в CEL-params.
type stubKVRender struct {
	secrets map[string]map[string]any
}

func (s stubKVRender) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if d, ok := s.secrets[path]; ok {
		return d, nil
	}
	return nil, context.Canceled // любой not-found-эрр; тест на success-путь
}

// vault() в CEL-params резолвится keeper-side: реальное значение секрета
// попадает в Params (а не ref-строка). Engine собран с KVReader (WithVault).
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
		t.Errorf("command = %q, want реальное значение секрета в Params", got)
	}
	if got := f["token"].GetStringValue(); got != "s3cr3t" {
		t.Errorf("token = %q, want s3cr3t (#field)", got)
	}
}

// TestRenderParams_NestedAndPassthrough — интерполяция в nested map/list,
// non-string-значения проходят насквозь.
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

// TestRenderParams_NativeTypeSingleBlock — одиночный ${expr} без обёртки даёт
// нативный тип (число), не строку (templating.md §5(а)).
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

// TestEvalWhere_EmptyTrue — пустой where: → true (нет фильтра).
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

// TestEvalWhere_SoulprintSelf — where: читает soulprint.self.* хоста.
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

// TestEvalWhere_SoulprintSelfChoirs — where: фильтрует по choir-членству хоста
// (ADR-044, S-T4): `X in soulprint.self.choirs`.
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
		t.Error("evalWhere = false для члена choir 'voters', want true")
	}

	outsider := soulprintSelfMap(&topology.HostFacts{SID: "b.example.com"})
	ok, err = evalWhere(e, "'voters' in soulprint.self.choirs", cel.Vars{SoulprintSelf: outsider})
	if err != nil {
		t.Fatalf("evalWhere (outsider): %v", err)
	}
	if ok {
		t.Error("evalWhere = true для хоста без choir-членств, want false")
	}
}

// TestEvalWhere_SoulprintSelfTraits — GUARD (ADR-060): where: таргетит по
// operator-set traits хоста (registry-проекция soulprint.self.traits). Скаляр
// `traits.namespace == 'dba-ns'` и список `'alice' in traits.owners` резолвятся
// БЕЗ AST-rewrite (traits — обычное map-поле под self). Покрывает match +
// исключение не-матча.
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
	// Хост вообще без traits (nil map → пустой объект под self.traits): обращение
	// к ключу даёт штатный no-such-key, предикат → false (не паника/ошибка).
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

// TestSoulprintSelfMap_Traits — GUARD (ADR-060): soulprint.self.traits всегда
// присутствует (пустой map при nil) и несёт operator-set значения как есть
// (scalar + list). Симметрия с hostFactsToMap проверяется ниже.
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

	// nil Traits → пустой map (не отсутствие ключа traits под self).
	bare := soulprintSelfMap(&topology.HostFacts{SID: "bare.example.com"})
	if m, ok := bare["traits"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("self.traits for nil-traits host = %v, want empty map", bare["traits"])
	}

	// Симметрия: элемент soulprint.hosts несёт те же traits.
	elem := hostFactsToMap(h)
	htraits, ok := elem["traits"].(map[string]any)
	if !ok || htraits["namespace"] != "dba-ns" {
		t.Errorf("hosts[].traits = %v, want согласованный с self", elem["traits"])
	}
}

// TestSoulprintSelfMap_TraitsRegistryWinsOverReported — GUARD (ADR-060):
// анти-спуфинг. operator-set traits живут в registry (HostFacts.Traits); если
// Soul-агент пришлёт reported-soulprint с ключом "traits", registry-проекция
// ОБЯЗАНА его перекрыть — Soul не может подменить operator-traits (таргетинг по
// ним = доверенное решение оператора). Симметрия с sid/covens-инвариантом
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
		t.Errorf("self.traits.namespace = %v, registry must override reported (анти-спуфинг)", traits["namespace"])
	}
}

// TestSoulprintSelfMap_NullReportedFacts — BUG E2E #3: при NULL reported
// soulprint (Soul-агент не репортит) soulprint.self.sid/.covens/.role всё равно
// доступны из roster (registry-проекция, ADR-018).
func TestSoulprintSelfMap_NullReportedFacts(t *testing.T) {
	h := &topology.HostFacts{
		SID:       "web-2.example.com",
		Coven:     []string{"prod", "web"},
		Role:      "replica",
		Soulprint: nil, // Soul ещё не прислал SoulprintReport
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

// TestSoulprintSelfMap_MergeReported — reported os/network доступны вместе с
// registry sid/covens/role.
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

// TestSoulprintSelfMap_RegistryWinsOverReported — если в reported-map затесался
// sid/covens, побеждает roster (registry — источник истины, ADR-018).
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

// TestSoulprintSelfMap_NoMutateRoster — soulprintSelfMap не портит host.Soulprint
// roster-а (deep-copy верхнего уровня).
func TestSoulprintSelfMap_NoMutateRoster(t *testing.T) {
	reported := map[string]any{"os": map[string]any{"family": "debian"}}
	h := &topology.HostFacts{SID: "h.example.com", Coven: []string{"prod"}, Soulprint: reported}

	_ = soulprintSelfMap(h)

	if _, leaked := reported["sid"]; leaked {
		t.Error("soulprintSelfMap замутировал host.Soulprint (добавил sid)")
	}
	if _, leaked := reported["covens"]; leaked {
		t.Error("soulprintSelfMap замутировал host.Soulprint (добавил covens)")
	}
}

// TestEvalWhere_AddReplicasNullFacts — регресс E2E #3: where из add_replicas
// рендерится без «no such key: sid» при NULL reported facts.
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
		t.Error("evalWhere = false, want true (web-2 не в replicas)")
	}
}

// TestSoulprintSelf_HostsSymmetry — self и элемент soulprint.hosts согласованы по
// sid/covens/role/choirs.
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
		t.Errorf("sid рассинхрон: self=%v hosts=%v", self["sid"], hostsEl["sid"])
	}
	if self["role"] != hostsEl["role"] {
		t.Errorf("role рассинхрон: self=%v hosts=%v", self["role"], hostsEl["role"])
	}
	assertListSymmetry(t, "covens", self["covens"], hostsEl["covens"])
	assertListSymmetry(t, "choirs", self["choirs"], hostsEl["choirs"])
}

func assertListSymmetry(t *testing.T, name string, selfV, hostsV any) {
	t.Helper()
	selfL, _ := selfV.([]any)
	hostsL, _ := hostsV.([]any)
	if len(selfL) != len(hostsL) {
		t.Fatalf("%s len рассинхрон: self=%v hosts=%v", name, selfL, hostsL)
	}
	for i := range selfL {
		if selfL[i] != hostsL[i] {
			t.Errorf("%s[%d] рассинхрон: self=%v hosts=%v", name, i, selfL[i], hostsL[i])
		}
	}
}
