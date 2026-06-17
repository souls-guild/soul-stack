package augur

// Авторизационный резолв AugurRequest (augur.md §6) — enforcement-точка
// брокера. Чистая функция от (sid, omen_name, query) + reader-ов реестра:
// решает, разрешён ли запрос, и какой Rite его разрешил. Сам fetch значения
// (vault-broker) и отправка reply — отдельные слои (broker.go / grpc-handler).
//
// Slice C (MVP-1) — source_type vault / prometheus / elk, delegate=false.
// Делегация (delegate=true, MVP-2) даёт Denied (см. [Resolve]).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// Decision — исход [Resolve]. Allowed=false → доступ запрещён, Reason несёт
// человекочитаемую причину (для AugurReply.error и audit-payload). При
// Allowed=true заполнены Omen и Query — нормализованный (через ParseRef)
// logical-путь, по которому брокер читает Vault.
//
// Query при Allowed=true — каноническое значение, по которому брокер делает
// fetch:
//   - vault: НОРМАЛИЗОВАННЫЙ logical-path (mount/rel), а не исходный raw-query —
//     enforcement и fetch обязаны работать по одному каноническому значению
//     (иначе secret//x обошёл бы allow-list);
//   - prometheus: promQL «как есть» (exact-match по Rite.allow.queries);
//   - elk: index «как есть» (exact-match по Rite.allow.indices).
type Decision struct {
	Allowed bool
	Reason  string
	Omen    *Omen
	Query   string
}

// denied — конструктор отказного решения (default-deny). Reason пробрасывается
// в AugurReply.error и audit `augur.access_denied`; секрет-значений в нём нет.
func denied(reason string) *Decision { return &Decision{Allowed: false, Reason: reason} }

// OmenReader / RiteReader / CovenReader — узкие поверхности реестров, нужные
// резолву. Сужение (вместо передачи *pgxpool.Pool) изолирует enforcement от
// CRUD-а и даёт fake в unit-тестах без подъёма PG. Реальные реализации —
// замыкания над [SelectOmenByName] / [SelectRitesBySubject] / soul-registry
// (см. grpc-handler wire-up).
type OmenReader interface {
	OmenByName(ctx context.Context, name string) (*Omen, error)
}

type RiteReader interface {
	RitesBySubject(ctx context.Context, sid string, covens []string) ([]*Rite, error)
}

// CovenReader резолвит covens по SID из АВТОРИТЕТНОГО registry (souls.coven[]),
// НЕ из payload запроса (augur.md §6.2: covens берутся из реестра, не из
// AugurRequest). Возврат [ErrSubjectUnknown] при отсутствии Soul-а в реестре.
type CovenReader interface {
	CovensBySID(ctx context.Context, sid string) ([]string, error)
}

// ErrSubjectUnknown — SID не найден в souls-реестре. Резолв трактует как
// Denied (нет авторитетного субъекта → нет grant-а), не как ERROR: для брокера
// это нормальный отказ, а не сбой Keeper-а.
var ErrSubjectUnknown = errors.New("augur: sid not found in souls registry")

// Resolve — авторизационный резолв (augur.md §6). default-deny: любая
// непройденная проверка → Decision{Allowed:false} + Reason, без чтения секрета.
//
// Шаги:
//  1. Omen существует (OmenByName). Нет → denied.
//  2. source_type ∈ {vault, prometheus, elk}. unknown → denied.
//  3. delegate-ветвь Slice C — только delegate=false (брокер). delegate=true
//     (MVP-2) → denied (минтинг/выдача cred — отдельный слайс).
//  4. covens по SID из registry (CovensBySID) — авторитетный источник.
//  5. Rite найден (RitesBySubject) на этот Omen. Нет → denied.
//  6. query ∈ Rite.allow, EXACT-match по форме source_type:
//     vault      — paths, ПОСЛЕ нормализации vault-path (иначе secret//x
//     обойдёт allow-list);
//     prometheus — queries, exact-match сырого promQL;
//     elk        — indices, exact-match сырого index.
//     Нет → denied.
//
// Возврат error (не Decision) — только на инфраструктурный сбой reader-а
// (PG недоступен и т.п.): caller отдаёт AugurReply{status:ERROR}. Семантический
// отказ авторизации — это Decision{Allowed:false}, error=nil.
//
// sid берётся из mTLS peer cert на стороне caller-а (grpc-handler), не из
// AugurRequest — сюда приходит уже авторитетный.
func Resolve(
	ctx context.Context,
	omens OmenReader,
	rites RiteReader,
	covens CovenReader,
	sid, omenName, query string,
) (*Decision, error) {
	omen, err := omens.OmenByName(ctx, omenName)
	if err != nil {
		if errors.Is(err, ErrOmenNotFound) {
			return denied(fmt.Sprintf("omen %q not found", omenName)), nil
		}
		return nil, fmt.Errorf("augur: resolve omen %q: %w", omenName, err)
	}

	switch omen.SourceType {
	case SourceVault, SourcePrometheus, SourceELK:
		// поддержано в этом слайсе.
	default:
		return denied(fmt.Sprintf("unknown source_type %q", omen.SourceType)), nil
	}

	covenList, err := covens.CovensBySID(ctx, sid)
	if err != nil {
		if errors.Is(err, ErrSubjectUnknown) {
			return denied("subject not registered"), nil
		}
		return nil, fmt.Errorf("augur: resolve covens for sid %q: %w", sid, err)
	}

	candidates, err := rites.RitesBySubject(ctx, sid, covenList)
	if err != nil {
		return nil, fmt.Errorf("augur: resolve rites for sid %q: %w", sid, err)
	}

	// Каноническое значение query, по которому идёт и enforcement, и fetch.
	// Для vault — нормализованный logical-path (иначе secret//x обходит
	// allow-list, см. vault.normalizeLogical); для prom/elk — query «как есть»
	// (exact-match сырого promQL/index).
	wantQuery, perr := canonicalQuery(omen.SourceType, query)
	if perr != nil {
		return denied(fmt.Sprintf("invalid query for source_type %q", omen.SourceType)), nil
	}

	for _, r := range candidates {
		if r.Omen != omenName {
			continue
		}
		if r.Delegate {
			// MVP-2 — здесь не выдаём cred/токен.
			continue
		}
		allowed, aerr := riteAllows(omen.SourceType, r, wantQuery)
		if aerr != nil {
			// Битый allow-JSONB у этого Rite — пропускаем его (не валит весь
			// резолв: другой Rite на тот же Omen может быть валиден). Insert-time
			// валидация (ValidateAllow) делает это редким, но БД могла прийти из
			// миграции/ручной правки.
			continue
		}
		if allowed {
			return &Decision{Allowed: true, Omen: omen, Query: wantQuery}, nil
		}
	}

	return denied("no rite grants this query"), nil
}

// canonicalQuery приводит сырой query к каноническому виду по форме source_type:
// vault — нормализованный logical-path (normalizeQueryPath); prom/elk — query
// без изменений (exact-match сырого значения). Пустой prom/elk-query отвергается
// (нечего матчить).
func canonicalQuery(src SourceType, query string) (string, error) {
	switch src {
	case SourceVault:
		return normalizeQueryPath(query)
	case SourcePrometheus, SourceELK:
		if query == "" {
			return "", fmt.Errorf("augur: empty query")
		}
		return query, nil
	default:
		return "", fmt.Errorf("augur: unknown source_type %q", src)
	}
}

// riteAllows — EXACT-match wantQuery против Rite.allow по форме source_type.
func riteAllows(src SourceType, r *Rite, wantQuery string) (bool, error) {
	switch src {
	case SourceVault:
		return riteAllowsVaultPath(r, wantQuery)
	case SourcePrometheus:
		return riteAllowsExact[allowPrometheus](r, wantQuery, func(a allowPrometheus) []string { return a.Queries })
	case SourceELK:
		return riteAllowsExact[allowELK](r, wantQuery, func(a allowELK) []string { return a.Indices })
	default:
		return false, fmt.Errorf("augur: unknown source_type %q", src)
	}
}

// riteAllowsExact — EXACT-match want против списка allow-значений, извлечённого
// из Rite.allow дженерик-распаковкой по форме source_type (prom: queries; elk:
// indices). Сравнение строгое строковое — никакой нормализации promQL/index не
// делаем (нормализация promQL семантически нетривиальна и сама может стать
// вектором обхода allow-list; exact-match — security-консервативный дефолт).
func riteAllowsExact[T any](r *Rite, want string, pick func(T) []string) (bool, error) {
	var a T
	if err := json.Unmarshal(r.Allow, &a); err != nil {
		return false, fmt.Errorf("augur: rite %d allow unmarshal: %w", r.ID, err)
	}
	for _, v := range pick(a) {
		if v == want {
			return true, nil
		}
	}
	return false, nil
}

// normalizeQueryPath приводит сырой vault-query к каноническому logical-path-у
// тем же механизмом, что и allow-пути: query может прийти как logical
// (`secret/keeper/x`), и его надо нормализовать через ParseRef. ParseRef ждёт
// `vault:`-префикс, поэтому добавляем его, если query без него (Soul шлёт
// logical-путь, не vault-ref).
func normalizeQueryPath(query string) (string, error) {
	ref := query
	if !hasVaultPrefix(query) {
		ref = "vault:" + query
	}
	return vault.ParseRef(ref)
}

// riteAllowsVaultPath — EXACT-match нормализованного wantPath против Rite.allow.
// allow.paths нормализуются тем же normalizeQueryPath: оператор мог записать
// путь как `vault:secret/x`, `secret/x` или с лишним слэшем — сравнение идёт
// по каноническому значению с обеих сторон (иначе тайпо в allow = silent
// обход/мисс).
func riteAllowsVaultPath(r *Rite, wantPath string) (bool, error) {
	var a allowVault
	if err := json.Unmarshal(r.Allow, &a); err != nil {
		return false, fmt.Errorf("augur: rite %d allow unmarshal: %w", r.ID, err)
	}
	for _, p := range a.Paths {
		got, err := normalizeQueryPath(p)
		if err != nil {
			// Битый путь в allow — игнорируем именно его, остальные проверяем.
			continue
		}
		if got == wantPath {
			return true, nil
		}
	}
	return false, nil
}

func hasVaultPrefix(s string) bool {
	const p = "vault:"
	return len(s) >= len(p) && s[:len(p)] == p
}
