package topology

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakePool — Querier-stub. Маршрутизирует QueryRow/Query по содержимому SQL:
// `incarnation` → spec, `souls` → roster, `incarnation_choir_voices` →
// choir-membership (ADR-044, S-T4). Чужой SQL — паника (тест-баг).
type fakePool struct {
	specJSON []byte
	specErr  error // напр. pgx.ErrNoRows для несуществующей incarnation

	rosterRows []rosterRow
	rosterErr  error // ошибка Query или итерации

	choirRows []choirVoiceRow
	choirErr  error // ошибка Query choir-voices
}

// choirVoiceRow — одна строка `incarnation_choir_voices` в порядке
// choirVoicesSQL (sid, choir_name, role). role — *string: nil эмулирует SQL NULL
// (role nullable, миграция 060), отличая «нет роли» от Go-строки "". Это ловит
// баг скана NULL в plain string (pgx «cannot scan NULL into *string»).
type choirVoiceRow struct {
	sid       string
	choirName string
	role      *string
}

// rosterRow — одна строка `souls`-roster-а: ровно поля rosterSQL по порядку.
type rosterRow struct {
	sid         string
	coven       []string
	traitsJSON  []byte     // nil = '{}' (jsonb NOT NULL DEFAULT) → пустой map; ADR-060
	status      string     // "" → дефолт "connected" в Scan (SQL-presence fallback)
	factsJSON   []byte     // nil = NULL soulprint
	collectedAt *time.Time // nil = NULL
	receivedAt  *time.Time // nil = NULL
}

func (p *fakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "FROM incarnation") {
		if p.specErr != nil {
			return errRow{err: p.specErr}
		}
		return specRow{spec: p.specJSON}
	}
	panic("fakePool.QueryRow: unexpected SQL: " + sql)
}

func (p *fakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM incarnation_choir_voices"):
		if p.choirErr != nil {
			return nil, p.choirErr
		}
		return &choirVoiceRows{rows: p.choirRows}, nil
	case strings.Contains(sql, "FROM souls"):
		if p.rosterErr != nil {
			return nil, p.rosterErr
		}
		return &rosterRows{rows: p.rosterRows}, nil
	default:
		panic("fakePool.Query: unexpected SQL: " + sql)
	}
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// specRow возвращает spec в единственный *[]byte-dest (incarnationSpecSQL).
type specRow struct{ spec []byte }

func (r specRow) Scan(dest ...any) error {
	*(dest[0].(*[]byte)) = r.spec
	return nil
}

// rosterRows прогоняет rosterRow за rosterRow, скан в порядке rosterSQL.
type rosterRows struct {
	rows []rosterRow
	idx  int
	err  error
}

func (r *rosterRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *rosterRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	status := row.status
	if status == "" {
		// Большинство кейсов не интересуются presence-снимком — дефолт
		// "connected", чтобы nil-lease SQL-fallback резолвера их пропускал.
		status = "connected"
	}
	*(dest[0].(*string)) = row.sid
	*(dest[1].(*[]string)) = row.coven
	*(dest[2].(*[]byte)) = row.traitsJSON
	*(dest[3].(*string)) = status
	*(dest[4].(*[]byte)) = row.factsJSON
	*(dest[5].(**time.Time)) = row.collectedAt
	*(dest[6].(**time.Time)) = row.receivedAt
	return nil
}

func (r *rosterRows) Err() error                                   { return r.err }
func (r *rosterRows) Close()                                       {}
func (r *rosterRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rosterRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rosterRows) Values() ([]any, error)                       { return nil, nil }
func (r *rosterRows) RawValues() [][]byte                          { return nil }
func (r *rosterRows) Conn() *pgx.Conn                              { return nil }

// choirVoiceRows прогоняет choirVoiceRow за choirVoiceRow, скан в порядке
// choirVoicesSQL (sid, choir_name).
type choirVoiceRows struct {
	rows []choirVoiceRow
	idx  int
	err  error
}

func (r *choirVoiceRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *choirVoiceRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*string)) = row.sid
	*(dest[1].(*string)) = row.choirName
	// role сканится в *string (nullable): nil-row.role → dest остаётся nil,
	// эмулируя SQL NULL без падения pgx (паритет с реальным skan-ом резолвера).
	*(dest[2].(**string)) = row.role
	return nil
}

func (r *choirVoiceRows) Err() error                                   { return r.err }
func (r *choirVoiceRows) Close()                                       {}
func (r *choirVoiceRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *choirVoiceRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *choirVoiceRows) Values() ([]any, error)                       { return nil, nil }
func (r *choirVoiceRows) RawValues() [][]byte                          { return nil }
func (r *choirVoiceRows) Conn() *pgx.Conn                              { return nil }

func newResolver(p *fakePool, logger *slog.Logger) *Resolver {
	return &Resolver{pool: p, logger: logger}
}

// newResolverWithLease — Resolver с fake lease-checker для presence-фазы
// (Variant A). lease-aware путь резолвера (фаза 2) тестируется без Redis.
func newResolverWithLease(p *fakePool, lease SoulLeaseChecker, logger *slog.Logger) *Resolver {
	return &Resolver{pool: p, lease: lease, logger: logger}
}

// fakeLease — SoulLeaseChecker-stub: alive — множество online-SID-ов, err —
// принудительная ошибка Redis (тест fail-safe-деградации на SQL-presence).
type fakeLease struct {
	alive    map[string]struct{}
	err      error
	gotSIDs  []string
	gotCalls int
}

func (l *fakeLease) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	l.gotCalls++
	l.gotSIDs = append([]string{}, sids...)
	if l.err != nil {
		return nil, l.err
	}
	out := make(map[string]struct{}, len(sids))
	for _, sid := range sids {
		if _, ok := l.alive[sid]; ok {
			out[sid] = struct{}{}
		}
	}
	return out, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func strPtr(s string) *string { return &s }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// --- LoadIncarnationHosts ---------------------------------------------

func TestLoadIncarnationHosts_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	specJSON := mustJSON(t, map[string]any{
		"hosts": []map[string]any{
			{"sid": "a.example.com", "role": "master"},
			{"sid": "b.example.com", "role": "replica"},
		},
	})
	p := &fakePool{
		specJSON: specJSON,
		rosterRows: []rosterRow{
			{
				sid:         "a.example.com",
				coven:       []string{"redis-prod", "db"},
				factsJSON:   mustJSON(t, map[string]any{"os": map[string]any{"family": "debian"}}),
				collectedAt: ptrTime(now.Add(-time.Minute)),
				receivedAt:  ptrTime(now.Add(-time.Minute)),
			},
			{
				sid:        "b.example.com",
				coven:      []string{"redis-prod"},
				factsJSON:  nil, // Soul ещё не прислал soulprint
				receivedAt: nil,
			},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(hosts))
	}

	a := hosts[0]
	if a.SID != "a.example.com" || a.Role != "master" {
		t.Errorf("host[0] = %+v, want master a.example.com", a)
	}
	if a.Soulprint == nil {
		t.Fatal("host[0].Soulprint = nil, want parsed map")
	}
	osMap, _ := a.Soulprint["os"].(map[string]any)
	if osMap["family"] != "debian" {
		t.Errorf("host[0] soulprint os.family = %v, want debian", osMap["family"])
	}
	if a.CollectedAt.IsZero() || a.ReceivedAt.IsZero() {
		t.Errorf("host[0] timestamps not populated: %+v", a)
	}

	b := hosts[1]
	if b.SID != "b.example.com" || b.Role != "replica" {
		t.Errorf("host[1] = %+v, want replica b.example.com", b)
	}
	if b.Soulprint != nil {
		t.Errorf("host[1].Soulprint = %v, want nil (NULL facts)", b.Soulprint)
	}
	if !b.ReceivedAt.IsZero() {
		t.Errorf("host[1].ReceivedAt = %v, want zero", b.ReceivedAt)
	}
}

func TestLoadIncarnationHosts_MissingIncarnation_EmptySlice(t *testing.T) {
	// PM-decision #3: несуществующая incarnation → пустой slice, НЕ ошибка.
	p := &fakePool{specErr: pgx.ErrNoRows, rosterRows: nil}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(hosts))
	}
}

func TestLoadIncarnationHosts_NoCandidates_EmptySlice(t *testing.T) {
	// Incarnation существует, но не-terminal/не-онбординг кандидатов нет
	// (фаза-1 SQL вернула пусто). → пустой slice, не ошибка.
	p := &fakePool{specJSON: mustJSON(t, map[string]any{}), rosterRows: nil}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(hosts))
	}
}

// --- presence-фаза (lease-aware, Variant A) ---------------------------

func TestLoadIncarnationHosts_LeaseAware_FiltersByLiveLease(t *testing.T) {
	// Фаза 2: кандидат с живым lease таргетится; без lease — отсеивается.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "online.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "offline.example.com", coven: []string{"redis-prod"}, status: "connected"},
		},
	}
	lease := &fakeLease{alive: map[string]struct{}{"online.example.com": {}}}
	r := newResolverWithLease(p, lease, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "online.example.com" {
		t.Fatalf("got %v, want [online.example.com]", sids(hosts))
	}
	if lease.gotCalls != 1 {
		t.Errorf("lease checked %d times, want 1 (batch)", lease.gotCalls)
	}
}

func TestLoadIncarnationHosts_LeaseAware_PresenceNotFromStatus(t *testing.T) {
	// Ключевой инвариант: presence решает lease, НЕ снимок souls.status.
	// disconnected-снимок с живым lease (idle-Soul, reconnect не отразился в PG)
	// → таргетируется; connected-снимок без lease (stale) → НЕ таргетируется.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "idle.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
			{sid: "stale.example.com", coven: []string{"redis-prod"}, status: "connected"},
		},
	}
	lease := &fakeLease{alive: map[string]struct{}{"idle.example.com": {}}}
	r := newResolverWithLease(p, lease, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "idle.example.com" {
		t.Fatalf("got %v, want [idle.example.com] (presence = lease, not status)", sids(hosts))
	}
}

func TestLoadIncarnationHosts_LeaseAware_RedisError_FallsBackToSQLSnapshot(t *testing.T) {
	// Fail-safe: ошибка Redis-проверки → деградация на SQL-presence-снимок
	// (status='connected'), а не падение прогона (no_hosts → error_locked).
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "conn.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "disc.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
		},
	}
	lease := &fakeLease{err: errors.New("redis down")}
	r := newResolverWithLease(p, lease, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "conn.example.com" {
		t.Fatalf("got %v, want [conn.example.com] (SQL snapshot fallback)", sids(hosts))
	}
	if !strings.Contains(buf.String(), "fail-safe") {
		t.Errorf("warn-лог без fail-safe-сообщения: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_NilLease_FallsBackToSQLSnapshot(t *testing.T) {
	// lease==nil (single-instance dev / unit) → SQL-presence-снимок.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "conn.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "disc.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "conn.example.com" {
		t.Fatalf("got %v, want [conn.example.com] (nil-lease SQL fallback)", sids(hosts))
	}
}

func TestLoadIncarnationHosts_RoleEmptyForUndeclaredHost(t *testing.T) {
	// ADR-008: хост, привязанный к incarnation вне declared-spec, имеет
	// declared-роль "". Резолвер не выдумывает роль из факта привязки.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{
			"hosts": []map[string]any{{"sid": "declared.example.com", "role": "master"}},
		}),
		rosterRows: []rosterRow{
			{sid: "declared.example.com", coven: []string{"redis-prod"}},
			{sid: "extra.example.com", coven: []string{"redis-prod"}},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "master" {
		t.Errorf("declared host role = %q, want master", hosts[0].Role)
	}
	if hosts[1].Role != "" {
		t.Errorf("undeclared host role = %q, want empty", hosts[1].Role)
	}
}

func TestLoadIncarnationHosts_ChoirMemberships(t *testing.T) {
	// ADR-044, S-T4: choir-членства хоста — стабильный per-host факт. Хост в
	// нескольких Choir-ах → несколько имён (детерминированный порядок из SQL);
	// хост без Voice-ов → nil Choirs (симметрия с пустой declared-ролью).
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
			{sid: "b.example.com", coven: []string{"redis-prod"}},
			{sid: "c.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			{sid: "a.example.com", choirName: "primaries"},
			{sid: "a.example.com", choirName: "voters"},
			{sid: "b.example.com", choirName: "replicas"},
			// c.example.com — без Voice-ов.
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if got := hosts[0].Choirs; len(got) != 2 || got[0] != "primaries" || got[1] != "voters" {
		t.Errorf("host[0].Choirs = %v, want [primaries voters]", got)
	}
	if got := hosts[1].Choirs; len(got) != 1 || got[0] != "replicas" {
		t.Errorf("host[1].Choirs = %v, want [replicas]", got)
	}
	if hosts[2].Choirs != nil {
		t.Errorf("host[2].Choirs = %v, want nil (no Voice-ов)", hosts[2].Choirs)
	}
}

func TestLoadIncarnationHosts_VoiceRoleOverridesSpec(t *testing.T) {
	// ADR-044 п.2: Choir поглощает declared-роль. voice.role (из
	// incarnation_choir_voices) побеждает spec.hosts[].role.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{
			"hosts": []map[string]any{
				{"sid": "a.example.com", "role": "spec-master"},
			},
		}),
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			{sid: "a.example.com", choirName: "primaries", role: strPtr("voice-master")},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "voice-master" {
		t.Errorf("role = %q, want voice-master (Voice побеждает spec)", hosts[0].Role)
	}
}

func TestLoadIncarnationHosts_SpecRoleFallbackWhenNoVoice(t *testing.T) {
	// ADR-044 п.2: spec.hosts[].role остаётся fallback-ом для хостов БЕЗ Voice
	// (bootstrap-create, wire-совместимость). Также fallback при Voice без role:
	//   - nullrole — Voice с SQL NULL role (AddVoice пишет NULL при опущенной
	//     роли, миграция 060). Это путь, на котором резолвер падал «cannot scan
	//     NULL into *string»; без фикса скана тест валится здесь, а не в assert-е.
	//   - emptyrole — Voice с role="" (Go-строка) → тоже fallback на spec.
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{
			"hosts": []map[string]any{
				{"sid": "novoice.example.com", "role": "spec-master"},
				{"sid": "nullrole.example.com", "role": "spec-replica"},
				{"sid": "emptyrole.example.com", "role": "spec-arbiter"},
			},
		}),
		rosterRows: []rosterRow{
			{sid: "novoice.example.com", coven: []string{"redis-prod"}},
			{sid: "nullrole.example.com", coven: []string{"redis-prod"}},
			{sid: "emptyrole.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			// novoice.example.com — без Voice-ов вообще.
			// nullrole.example.com — Voice с SQL NULL role (role: nil) → fallback.
			{sid: "nullrole.example.com", choirName: "voters", role: nil},
			// emptyrole.example.com — Voice с role="" → fallback на spec.
			{sid: "emptyrole.example.com", choirName: "voters", role: strPtr("")},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "spec-master" {
		t.Errorf("host[0] role = %q, want spec-master (нет Voice → fallback на spec)", hosts[0].Role)
	}
	if hosts[1].Role != "spec-replica" {
		t.Errorf("host[1] role = %q, want spec-replica (NULL voice.role → fallback на spec)", hosts[1].Role)
	}
	if hosts[2].Role != "spec-arbiter" {
		t.Errorf("host[2] role = %q, want spec-arbiter (пустой voice.role → fallback на spec)", hosts[2].Role)
	}
}

func TestLoadIncarnationHosts_MultiChoirRoleConflict_FirstBySortNameWins(t *testing.T) {
	// ADR-044 п.2: SID — Voice в нескольких Choir-ах с РАЗНЫМИ непустыми role.
	// HostFacts.Role — скаляр → детерминированно берём role из ПЕРВОГО Choir-а
	// по сортировке choir_name + WARN о конфликте. SQL отдаёт ORDER BY choir_name
	// ASC, поэтому fake подаёт строки уже в этом порядке.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{
			"hosts": []map[string]any{
				{"sid": "a.example.com", "role": "spec-role"},
			},
		}),
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			// "alpha" < "beta" лексикографически → побеждает alpha-role.
			{sid: "a.example.com", choirName: "alpha", role: strPtr("alpha-role")},
			{sid: "a.example.com", choirName: "beta", role: strPtr("beta-role")},
		},
	}
	r := newResolver(p, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "alpha-role" {
		t.Errorf("role = %q, want alpha-role (первый Choir по сорт. имени)", hosts[0].Role)
	}
	out := buf.String()
	if !strings.Contains(out, "conflict") {
		t.Errorf("warn-лог без сообщения о конфликте: %q", out)
	}
	if !strings.Contains(out, "a.example.com") {
		t.Errorf("warn-лог без SID конфликтующего хоста: %q", out)
	}
	if !strings.Contains(out, "beta-role") {
		t.Errorf("warn-лог без конфликтующей role: %q", out)
	}
}

func TestLoadIncarnationHosts_StaleSoulprintWarns(t *testing.T) {
	// PM-decision #2: received_at < now-10m → warn, прогон НЕ блокируется.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := time.Now().UTC().Add(-30 * time.Minute)
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "stale.example.com", coven: []string{"redis-prod"}, receivedAt: ptrTime(old)},
		},
	}
	r := newResolver(p, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("len(hosts) = %d, want 1 (stale НЕ блокирует)", len(hosts))
	}
	if !strings.Contains(buf.String(), "stale.example.com") {
		t.Errorf("warn-лог не содержит SID устаревшего хоста: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "устарел") {
		t.Errorf("warn-лог без ожидаемого сообщения: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_FreshSoulprintNoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fresh := time.Now().UTC().Add(-time.Minute)
	p := &fakePool{
		specJSON: mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{
			{sid: "fresh.example.com", coven: []string{"redis-prod"}, receivedAt: ptrTime(fresh)},
			{sid: "neverreported.example.com", coven: []string{"redis-prod"}}, // zero received_at
		},
	}
	r := newResolver(p, logger)

	if _, err := r.LoadIncarnationHosts(context.Background(), "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("неожиданный warn для свежих/неотчитавшихся хостов: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_MalformedSpecRolesIgnored(t *testing.T) {
	// spec freeform: битый/неожиданный hosts → роли "", не ошибка.
	p := &fakePool{
		specJSON:   []byte(`{"hosts": "not-an-array"}`),
		rosterRows: []rosterRow{{sid: "a.example.com", coven: []string{"redis-prod"}}},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "" {
		t.Errorf("role = %q, want empty for malformed spec", hosts[0].Role)
	}
}

func TestLoadIncarnationHosts_BadSoulprintJSONErrors(t *testing.T) {
	// Битый soulprint_facts JSONB — это data-corruption, не штатный случай:
	// резолвер обязан вернуть ошибку, а не молча отдать пустой soulprint.
	p := &fakePool{
		specJSON:   mustJSON(t, map[string]any{}),
		rosterRows: []rosterRow{{sid: "a.example.com", coven: []string{"redis-prod"}, factsJSON: []byte(`{bad`)}},
	}
	r := newResolver(p, nil)

	_, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err == nil {
		t.Fatal("LoadIncarnationHosts вернул nil err на битом soulprint JSON")
	}
}

// --- parseDeclaredRoles -----------------------------------------------

func TestParseDeclaredRoles(t *testing.T) {
	cases := []struct {
		name string
		json string
		want map[string]string
	}{
		{"empty", "", map[string]string{}},
		{"empty-object", "{}", map[string]string{}},
		{"no-hosts", `{"replicas": 3}`, map[string]string{}},
		{"two-roles", `{"hosts":[{"sid":"a","role":"master"},{"sid":"b","role":"replica"}]}`,
			map[string]string{"a": "master", "b": "replica"}},
		{"role-without-sid-skipped", `{"hosts":[{"role":"master"}]}`, map[string]string{}},
		{"sid-without-role-skipped", `{"hosts":[{"sid":"a"}]}`, map[string]string{}},
		{"malformed", `{bad`, map[string]string{}},
		{"hosts-wrong-type", `{"hosts":"x"}`, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDeclaredRoles([]byte(tc.json))
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("roles[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// --- FilterByCovens ---------------------------------------------------

// TestFilterByCovens проверяет AND-семантику фильтра (ADR-040 amendment
// 2026-05-27 «Multi-label семантика внутри одного списка»; orchestration.md §3).
// Хост попадает в результат тогда и только тогда, когда у него присутствуют ВСЕ
// перечисленные метки фильтра.
func TestFilterByCovens(t *testing.T) {
	hosts := []*HostFacts{
		{SID: "a", Coven: []string{"redis-prod", "db", "eu"}},          // db + eu
		{SID: "b", Coven: []string{"redis-prod", "cache"}},             // только cache
		{SID: "c", Coven: []string{"redis-prod", "eu"}},                // только eu
		{SID: "d", Coven: []string{"redis-prod", "db", "cache"}},       // db + cache
		{SID: "e", Coven: []string{"redis-prod", "db", "cache", "eu"}}, // все три
	}
	r := &Resolver{}

	t.Run("empty-required-returns-all", func(t *testing.T) {
		got := r.FilterByCovens(hosts, nil)
		if len(got) != 5 {
			t.Errorf("len = %d, want 5", len(got))
		}
	})

	t.Run("single-coven", func(t *testing.T) {
		// Single-label AND тривиально совпадает с любой формой фильтра.
		got := r.FilterByCovens(hosts, []string{"db"})
		want := []string{"a", "d", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("multi-coven-AND", func(t *testing.T) {
		// AND-пересечение: db + cache → только хосты, у которых ОБЕ метки.
		// Раньше (OR) возвращало бы {a, b, d, e}; теперь — только {d, e}.
		got := r.FilterByCovens(hosts, []string{"db", "cache"})
		want := []string{"d", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v (AND-пересечение)", sids(got), want)
		}
	})

	t.Run("multi-coven-AND-three-labels", func(t *testing.T) {
		// Тройной AND: db + cache + eu → только хост со всеми тремя метками.
		got := r.FilterByCovens(hosts, []string{"db", "cache", "eu"})
		want := []string{"e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("multi-coven-AND-no-host-has-all", func(t *testing.T) {
		// Хост {db} + хост {cache} + фильтр [db, cache]: ни один по отдельности
		// не несёт обеих меток → пустой результат (раньше OR давал {a, b}).
		isolated := []*HostFacts{
			{SID: "only-db", Coven: []string{"db"}},
			{SID: "only-cache", Coven: []string{"cache"}},
		}
		got := r.FilterByCovens(isolated, []string{"db", "cache"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty (AND fail-closed)", sids(got))
		}
	})

	t.Run("coven-matching-all", func(t *testing.T) {
		got := r.FilterByCovens(hosts, []string{"eu"})
		want := []string{"a", "c", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("no-match", func(t *testing.T) {
		got := r.FilterByCovens(hosts, []string{"nonexistent"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty", sids(got))
		}
	})

	t.Run("multi-coven-one-missing", func(t *testing.T) {
		// Все хосты несут redis-prod, но nonexistent — никто; AND даёт пусто.
		got := r.FilterByCovens(hosts, []string{"redis-prod", "nonexistent"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty (одна метка отсутствует у всех)", sids(got))
		}
	})
}

// equalSIDs — упорядоченное сравнение SID-списков (резолвер возвращает в порядке
// исходного roster-а, поэтому порядок детерминирован).
func equalSIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sids(hosts []*HostFacts) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.SID
	}
	return out
}
