package incarnation

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// Sentinel-ошибки prepare-фазы апгрейда (резолв цели до транзакции).
// Отделены от tx-уровневых sentinel-ов [UpgradeStateSchema]:
//   - ErrServiceNotRegistered → caller маппит в internal-error / 500
//     (сервис incarnation должен быть в реестре сервисов; управляется через
//     service.* API, ADR-029).
//   - ErrUpgradeNoop           → 422 / validation-failed (апгрейдить нечего:
//     то же ref И та же схема).
//   - ErrDowngradeViaRef       → 409 / incarnation-locked (целевой ref несёт
//     схему ниже текущей; forward-only, ADR-019). Ранний guard до загрузки
//     цепочки; tx-уровневый [ErrDowngradeUnsupported] остаётся защитой от
//     гонки resolve↔FOR UPDATE.
//   - ErrTargetSnapshotInvalid → internal-error / 500 (снапшот без манифеста).
//   - ErrLoadTargetSnapshot    → internal-error / 500 (git/loader-фейл).
//   - ErrBuildEvaluator        → internal-error / 500 (CEL-инициализация).
//
// [artifact.ErrMigrationChainBroken] прокидывается из LoadMigrationChain
// как есть (caller маппит в 422 / validation-failed): incarnation не тронут,
// проблема в самом таргете.
var (
	ErrServiceNotRegistered  = errors.New("incarnation: service is not registered (manage via service.* API, ADR-029)")
	ErrUpgradeNoop           = errors.New("incarnation: to_version matches current — nothing to upgrade")
	ErrDowngradeViaRef       = errors.New("incarnation: to_version downgrades state_schema_version (forward-only, ADR-019)")
	ErrTargetSnapshotInvalid = errors.New("incarnation: target service snapshot has no manifest")
	ErrLoadTargetSnapshot    = errors.New("incarnation: load target service snapshot failed")
	ErrBuildEvaluator        = errors.New("incarnation: build migration evaluator failed")
	ErrLoadMigrationChain    = errors.New("incarnation: load migration chain failed")
)

// ServiceResolver резолвит git-координаты service-репо по имени сервиса
// (`incarnation.service` → реестр сервисов в БД, ADR-029). Узкое подмножество
// scenario.ServiceRegistry; реальный резолвер удовлетворяет структурно.
type ServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ServiceSnapshotLoader материализует снапшот целевого service-ref-а (для
// чтения `state_schema_version` из service.yml) и собирает цепочку
// state_schema-миграций current→target. Узкое подмножество
// [artifact.ServiceLoader]; реальный загрузчик удовлетворяет структурно.
type ServiceSnapshotLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	LoadMigrationChain(art *artifact.ServiceArtifact, from, to int) (statemigrate.Chain, error)
}

// PrepareUpgrade — оркестрация резолва upgrade-цели (Variant C): из текущего
// inc и целевого to_version собирает готовый [UpgradeInput] для
// [UpgradeStateSchema]. Чистая от HTTP/MCP-транспорта: возвращает
// типизированные sentinel-ошибки, каждый caller (REST-handler / MCP-tool)
// маппит их в свой error-формат сам.
//
// Шаги (порядок выверен по REST-handler-у IncarnationHandler.Upgrade):
//  1. Resolve(inc.Service) → git-координаты; .Ref переопределяется на toVersion.
//  2. Load(targetRef) → снапшот; Manifest.StateSchemaVersion = target.
//  3. No-op detection: тот же ref И та же схема → [ErrUpgradeNoop].
//  4. Downgrade-guard: target < current → [ErrDowngradeViaRef] (forward-only).
//  5. LoadMigrationChain(art, current, target) → цепочка (пустая = ref-bump).
//  6. NewEvaluator → migration-CEL.
//  7. Собрать UpgradeInput (ApplyID / ChangedByAID передаёт caller).
//
// inc приходит уже загруженным (caller делает SelectByName сам — для
// 404-семантики и FOR UPDATE-гонки). evaluator/chain здесь только готовятся;
// атомарное применение — в [UpgradeStateSchema].
func PrepareUpgrade(
	ctx context.Context,
	resolver ServiceResolver,
	loader ServiceSnapshotLoader,
	inc *Incarnation,
	toVersion string,
	applyID string,
	changedByAID *string,
) (UpgradeInput, error) {
	ref, ok := resolver.Resolve(inc.Service)
	if !ok {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrServiceNotRegistered, inc.Service)
	}
	ref.Ref = toVersion

	art, err := loader.Load(ctx, ref)
	if err != nil {
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	// Узкий интерфейс не гарантирует Manifest != nil контрактом (мок может
	// вернуть art без манифеста): defensive перед разыменованием.
	if art == nil || art.Manifest == nil {
		return UpgradeInput{}, ErrTargetSnapshotInvalid
	}
	target := art.Manifest.StateSchemaVersion
	current := inc.StateSchemaVersion

	// No-op: ровно тот же ref И та же схема — апгрейдить нечего. Смена ref при
	// той же схеме (target == current, toVersion != inc.ServiceVersion) —
	// легитимный ref-bump (chain пустой, UpgradeStateSchema выполнит no-op).
	if toVersion == inc.ServiceVersion && target == current {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrUpgradeNoop, toVersion)
	}

	// Downgrade через git-ref (целевая схема < текущей): forward-only
	// (ADR-019). Ранний guard убирает впустую-вызов LoadMigrationChain (на
	// from>to загрузчик отдаёт обычную ошибку, не ErrMigrationChainBroken).
	if target < current {
		return UpgradeInput{}, fmt.Errorf("%w: %s", ErrDowngradeViaRef, toVersion)
	}

	chain, err := loader.LoadMigrationChain(art, current, target)
	if err != nil {
		if errors.Is(err, artifact.ErrMigrationChainBroken) {
			// Цепочка к target неполна — запрошенный to_version недостижим.
			// Прокидываем как есть: семантический отказ запроса (caller → 422).
			return UpgradeInput{}, err
		}
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrLoadMigrationChain, err)
	}

	ev, err := statemigrate.NewEvaluator()
	if err != nil {
		return UpgradeInput{}, fmt.Errorf("%w: %v", ErrBuildEvaluator, err)
	}

	return UpgradeInput{
		Name:             inc.Name,
		TargetServiceVer: toVersion,
		TargetSchemaVer:  target,
		Chain:            chain,
		Evaluator:        ev,
		ApplyID:          applyID,
		ChangedByAID:     changedByAID,
	}, nil
}
