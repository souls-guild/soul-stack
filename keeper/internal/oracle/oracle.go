// Package oracle — Keeper-side reactor-роутер beacons-контура (ADR-030, срез
// S2). Принимает Portent (beacon-событие от Soul-а), матчит его с реестром
// Decree и ставит named-scenario в work-queue (ADR-027). Сам apply не
// исполняет — только маршрутизирует.
//
// Состав среза S2:
//   - Vigil / Decree / Fire — runtime-типы реестров `vigils` / `decrees` /
//     `oracle_fires` (миграция 041);
//   - repository (crud.go): SelectActiveVigilsForSubject (резолв VigilSnapshot),
//     SelectDecreesByBeacon (горячий путь match), cooldown read/record;
//   - match-логика (match.go): subject-match + where-CEL + cooldown-check,
//     default-deny.
//
// Безопасность (ADR-030(b)): Portent — недоверенный вход (Soul может быть
// скомпрометирован). Защита слоями: default-deny Decree + субъектная привязка
// (coven XOR sid) + action = ТОЛЬКО named scenario (whitelist) + cooldown
// (loop-prevention). SID субъекта — авторитетно из mTLS peer cert, НЕ из
// PortentEvent.sid (echo).
//
// Что НЕ в S2 (последующие срезы): OpenAPI/MCP CRUD Vigil/Decree + RBAC-perms
// (S3); circuit-breaker + метрики (S4); inotify / soul_beacon-плагины /
// typed-payload (S5).
package oracle

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное репозиторию oracle.
// Симметрично [augur.ExecQueryRower] / [applyrun.ExecQueryRower]: unit-тесты
// ходят через fake без подъёма PG, production даёт реальный pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// Vigil — runtime-представление строки реестра `vigils` (Soul-side проверка,
// ADR-030). Субъект — строго XOR: ровно одно из Coven / SID непусто (CHECK
// vigils_subject_xor). Params — сырой JSONB (форма зависит от CheckAddr,
// валидируется на service-слое S3). Read-only по конструкции Vigil-а
// гарантируется Soul-стороной (S1), не этим типом.
type Vigil struct {
	Name         string          `json:"name"`
	Coven        []string        `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	IntervalSpec string          `json:"interval"`
	CheckAddr    string          `json:"check"`
	Params       json.RawMessage `json:"params"`
	Enabled      bool            `json:"enabled"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
}

// Decree — runtime-представление строки реестра `decrees` (правило reactor,
// ADR-030). Default-deny: нет матчащего Decree → событие не вызывает действия.
// Субъект — строго XOR (как Rite): SubjectCoven ИЛИ SubjectSID непусто.
// WhereCEL — опц. предикат над payload события (event.data); nil/пустой →
// всегда match (субъект уже отфильтровал). IncarnationName — таргет-incarnation
// реакции (РЕШЕНИЕ #1, вариант b): ServiceRef сценария резолвится ИЗ неё на
// enqueue-е, а не дублируется в Decree; оно же — корневая Coven-метка субъекта
// (ADR-008), по которой делается membership-проверка. ActionScenario — named
// scenario (whitelist; raw-команда отвергнута, ADR-030(b)). Cooldown —
// duration-строка (config.ParseDuration), минимальный интервал между
// срабатываниями per-(decree, subject).
type Decree struct {
	Name            string          `json:"name"`
	OnBeacon        string          `json:"on_beacon"`
	WhereCEL        *string         `json:"where_cel,omitempty"`
	SubjectCoven    []string        `json:"subject_coven,omitempty"`
	SubjectSID      *string         `json:"subject_sid,omitempty"`
	IncarnationName string          `json:"incarnation_name"`
	ActionScenario  string          `json:"action_scenario"`
	ActionInput     json.RawMessage `json:"action_input"`
	Cooldown        string          `json:"cooldown"`
	Enabled         bool            `json:"enabled"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	CreatedByAID    *string         `json:"created_by_aid,omitempty"`
}
