package serviceregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// === B5 кейс 4: ParseProvisioningMethods (config-error на пустом) ===

func TestParseProvisioningMethods_Valid(t *testing.T) {
	cases := []struct {
		csv  string
		want []string
	}{
		{"user,ldap,oidc", []string{"ldap", "oidc", "user"}},
		{" User , LDAP ", []string{"ldap", "user"}}, // trim + lowercase + dedup-домен
		{"user,user,user", []string{"user"}},        // dedup
		{"oidc,,,user", []string{"oidc", "user"}},   // пустые элементы отброшены
	}
	for _, c := range cases {
		set, err := ParseProvisioningMethods(c.csv)
		if err != nil {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want nil", c.csv, err)
			continue
		}
		got := sortedSet(set)
		if !equalStrs(got, c.want) {
			t.Errorf("ParseProvisioningMethods(%q) = %v, want %v", c.csv, got, c.want)
		}
	}
}

// TestParseProvisioningMethods_Empty — пустой/пробельный/только-запятые → config-
// error ErrEmptyProvisioningMethods (anti-lockout).
func TestParseProvisioningMethods_Empty(t *testing.T) {
	for _, csv := range []string{"", "   ", ",  ,", ",,,"} {
		_, err := ParseProvisioningMethods(csv)
		if !errors.Is(err, ErrEmptyProvisioningMethods) {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want ErrEmptyProvisioningMethods", csv, err)
		}
	}
}

// TestParseProvisioningMethods_InvalidMethod — метод вне {user,ldap,oidc} (в т.ч.
// bootstrap/system, которые НЕЛЬЗЯ задать в политике) → ErrInvalidProvisioningMethod.
func TestParseProvisioningMethods_InvalidMethod(t *testing.T) {
	for _, csv := range []string{"bootstrap", "system", "user,bootstrap", "saml", "ldap,unknown"} {
		_, err := ParseProvisioningMethods(csv)
		if !errors.Is(err, ErrInvalidProvisioningMethod) {
			t.Errorf("ParseProvisioningMethods(%q) err=%v, want ErrInvalidProvisioningMethod", csv, err)
		}
	}
}

// === B5 кейс 4: PoolSource.Load на пустом ключе → ошибка (снимок не публикуется) ===

// settingPool — fake ExecQueryRower для PoolSource.Load: ListServices даёт пустой
// каталог, GetSetting возвращает заданное значение по ключу (или ErrNoRows).
type settingPool struct {
	// values по ключу keeper_settings; отсутствие ключа → pgx.ErrNoRows.
	values map[string]string
}

func (p settingPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p settingPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// GetSetting: SELECT ... FROM keeper_settings WHERE key = $1.
	if len(args) == 1 {
		key, _ := args[0].(string)
		v, ok := p.values[key]
		if !ok {
			return settingScanRow{err: pgx.ErrNoRows}
		}
		return settingScanRow{key: key, value: v}
	}
	return settingScanRow{err: pgx.ErrNoRows}
}

func (p settingPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	// ListServices: пустой каталог.
	return &emptyRows{}, nil
}

type settingScanRow struct {
	key, value string
	err        error
}

func (r settingScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	// keeper_settings: key, value, updated_by_aid, updated_at.
	if len(dest) >= 4 {
		*dest[0].(*string) = r.key
		*dest[1].(*string) = r.value
		// dest[2] (*updatedByAID) и dest[3] (*time.Time) — оставляем zero.
		if tp, ok := dest[3].(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

type emptyRows struct{ pgx.Rows }

func (*emptyRows) Next() bool { return false }
func (*emptyRows) Err() error { return nil }
func (*emptyRows) Close()     {}

// TestPoolSourceLoad_ProvisioningKeyAbsent — ключа нет → политика не задана
// (nil-map, всё разрешено). B5 кейс 7.
func TestPoolSourceLoad_ProvisioningKeyAbsent(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{}}}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap.provisioningMethods != nil {
		t.Errorf("provisioningMethods = %v, want nil (ключ отсутствует → всё разрешено)", snap.provisioningMethods)
	}
}

// TestPoolSourceLoad_ProvisioningKeyValid — заданный валидный ключ парсится в set.
func TestPoolSourceLoad_ProvisioningKeyValid(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{
		SettingProvisioningAllowedMethods: "user,ldap",
	}}}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !snap.provisioningMethods["user"] || !snap.provisioningMethods["ldap"] || snap.provisioningMethods["oidc"] {
		t.Errorf("provisioningMethods = %v, want {user,ldap}", snap.provisioningMethods)
	}
}

// TestPoolSourceLoad_ProvisioningKeyEmpty — ключ ЗАДАН но пустой → Load возвращает
// ошибку (битый снимок НЕ публикуется; на старте NewHolder → fatal, anti-lockout).
// B5 кейс 4.
func TestPoolSourceLoad_ProvisioningKeyEmpty(t *testing.T) {
	src := PoolSource{DB: settingPool{values: map[string]string{
		SettingProvisioningAllowedMethods: "  , ,",
	}}}
	if _, err := src.Load(context.Background()); !errors.Is(err, ErrEmptyProvisioningMethods) {
		t.Fatalf("Load on empty key err=%v, want ErrEmptyProvisioningMethods", err)
	}
}

// === B5 кейсы 5/7: Holder-геттеры ===

// snapProv — снимок с заданной политикой (set non-nil) либо без неё (nil).
func snapProv(methods map[string]bool) *Snapshot {
	return &Snapshot{services: map[string]ServiceEntry{}, provisioningMethods: methods}
}

// TestHolder_ProvisioningMethodAllowed_BootstrapAlwaysTrue — bootstrap/system
// проходят всегда, даже при политике {} / без них. B5 кейс 5.
func TestHolder_ProvisioningMethodAllowed_BootstrapAlwaysTrue(t *testing.T) {
	// Политика без user/ldap/oidc невозможна (пустой набор = config-error), но
	// даже при ограничивающей {oidc} bootstrap/system должны проходить.
	src := &fakeSnapSource{snap: snapProv(map[string]bool{"oidc": true})}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	for _, m := range []string{"bootstrap", "system"} {
		if !h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = false, want true (никогда не гейтится)", m)
		}
	}
	// user/ldap при политике {oidc} — запрещены.
	for _, m := range []string{"user", "ldap"} {
		if h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = true при политике {oidc}, want false", m)
		}
	}
	if !h.ProvisioningMethodAllowed("oidc") {
		t.Error("ProvisioningMethodAllowed(oidc) = false при политике {oidc}, want true")
	}
}

// TestHolder_ProvisioningPolicy_DefaultAllowAll — политика не задана (nil-map) →
// все методы разрешены, GET-проекция policy_set=false. B5 кейс 7.
func TestHolder_ProvisioningPolicy_DefaultAllowAll(t *testing.T) {
	src := &fakeSnapSource{snap: snapProv(nil)}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	for _, m := range []string{"user", "ldap", "oidc", "bootstrap"} {
		if !h.ProvisioningMethodAllowed(m) {
			t.Errorf("ProvisioningMethodAllowed(%q) = false при не заданной политике, want true (back-compat)", m)
		}
	}
	methods, set := h.ProvisioningPolicy()
	if set {
		t.Errorf("ProvisioningPolicy set=true, want false (политика не задана)")
	}
	if methods != nil {
		t.Errorf("ProvisioningPolicy methods=%v, want nil", methods)
	}
}

// TestHolder_ProvisioningPolicy_Set — заданная политика отдаётся отсортированным
// списком + set=true.
func TestHolder_ProvisioningPolicy_Set(t *testing.T) {
	src := &fakeSnapSource{snap: snapProv(map[string]bool{"user": true, "ldap": true})}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	methods, set := h.ProvisioningPolicy()
	if !set {
		t.Fatal("ProvisioningPolicy set=false, want true")
	}
	if !equalStrs(methods, []string{"ldap", "user"}) {
		t.Errorf("ProvisioningPolicy methods=%v, want [ldap user]", methods)
	}
}

// TestHolder_ProvisioningMethodAllowed_NilReceiver — nil-Holder → true (gate не
// сконфигурирован, back-compat).
func TestHolder_ProvisioningMethodAllowed_NilReceiver(t *testing.T) {
	var h *Holder
	if !h.ProvisioningMethodAllowed("user") {
		t.Error("nil-Holder ProvisioningMethodAllowed(user) = false, want true (back-compat)")
	}
}

// --- helpers ---

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
