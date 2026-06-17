// Package choir — реестр Choir/Voice в Postgres (таблицы `incarnation_choirs` +
// `incarnation_choir_voices`, 060_create_choirs.up.sql) под ADR-044.
//
// Choir — именованная топология хостов ВНУТРИ инкарнации (declared-«партия
// хора»); Voice — членство конкретного SID в конкретном Choir-е. Три РАЗНЫХ
// слоя (ADR-044 пункт 1): membership = `souls.coven[]`, coven = стабильные теги
// (ADR-008), Choir = позиция хоста внутри инкарнации. Choir не схлопывается ни с
// membership, ни с coven.
//
// Источник правды declared-топологии — эти таблицы, НЕ `incarnation.state`
// (state коммитится только под cross-host barrier, ADR-044 пункт 4). `voice.role`
// поглощает declared-роль `incarnation.spec.hosts[].role` (ADR-044 пункт 2).
//
// Инвариант членства (ADR-044 пункт 3): Voice создаётся только для SID, который
// УЖЕ член этой инкарнации — его `souls.coven[]` содержит `incarnation.name`.
// Один SID легально является Voice в Choir-ах РАЗНЫХ инкарнаций (PK включает
// incarnation_name; глобального UNIQUE(sid) нет).
//
// Package scope (S-T2): транзакционный CRUD Choir/Voice по паттерну
// `incarnation/hosts.go` (SELECT FOR UPDATE → mutate → commit; валидация
// членства). API-эндпоинты / RBAC / audit — S-T3; резолвер `choirs[]` в
// soulprint — S-T4.
package choir

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// choirNamePattern — формат имени Choir, совпадает с CHECK
// `incarnation_choirs_name_format` миграции 060_create_choirs.up.sql (kebab/snake, начинается с
// буквы).
var choirNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidChoirName сообщает, соответствует ли имя [choirNamePattern].
func ValidChoirName(name string) bool {
	return choirNamePattern.MatchString(name)
}

// Choir — runtime-представление строки `incarnation_choirs`. MinSize/MaxSize —
// опциональные лимиты размера партии (declared под typed-input S-T1; на S-T2
// хранятся, кардинальность Voice-ов на уровне CRUD мягко не форсируется —
// валидация min/max применяется в S-T1 typed-input-слое).
type Choir struct {
	IncarnationName string
	ChoirName       string
	Description     *string
	MinSize         *int
	MaxSize         *int
	CreatedAt       time.Time
	CreatedByAID    *string
}

// Voice — runtime-представление строки `incarnation_choir_voices` (членство SID
// в Choir-е). Role — поглощённая declared-роль (ADR-044 пункт 2, nullable);
// Position — порядковый индекс внутри партии (nullable, напр. seed-узел).
type Voice struct {
	IncarnationName string
	ChoirName       string
	SID             string
	Role            *string
	Position        *int
	AddedAt         time.Time
	AddedByAID      *string
}

// Sentinel-ошибки CRUD-слоя.
//   - ErrIncarnationNotFound — нет строки incarnation по имени (404).
//   - ErrChoirNotFound       — нет Choir по паре (incarnation_name, choir_name).
//   - ErrChoirExists         — Choir с таким именем уже есть в инкарнации (409).
//   - ErrVoiceNotFound       — нет Voice по тройке PK (для RemoveVoice).
//   - ErrVoiceExists         — Voice для этого SID в этом Choir-е уже есть (409).
//   - ErrInvalidChoirName    — choir_name не матчит [choirNamePattern].
//   - ErrInvalidSizeBounds   — min_size > max_size (или ≤ 0) при создании.
var (
	ErrIncarnationNotFound = errors.New("choir: incarnation not found")
	ErrChoirNotFound       = errors.New("choir: choir not found")
	ErrChoirExists         = errors.New("choir: choir already exists in incarnation")
	ErrVoiceNotFound       = errors.New("choir: voice not found")
	ErrVoiceExists         = errors.New("choir: voice already exists in choir")
	ErrInvalidChoirName    = errors.New("choir: invalid choir name")
	ErrInvalidSizeBounds   = errors.New("choir: invalid min/max size bounds")
)

// ErrNotMembers — переданные SID-ы НЕ являются членами инкарнации (их
// `souls.coven[]` не содержит `incarnation.name`). Инвариант ADR-044 пункт 3.
// Handler-сторона (S-T3) маппит в 422; нарушившие SID-ы — в .Missing.
//
// Missing включает как SID-ы, которых нет в реестре `souls` вовсе, так и SID-ы,
// которые есть, но не члены данной инкарнации — для оператора разница не
// существенна (в обоих случаях Voice создавать нельзя), а отдельные классы
// усложнили бы контракт без пользы.
type ErrNotMembers struct {
	Incarnation string
	Missing     []string
}

func (e *ErrNotMembers) Error() string {
	return fmt.Sprintf("choir: %d SID(s) not members of incarnation %q: %v",
		len(e.Missing), e.Incarnation, e.Missing)
}
