package incarnation

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// Sentinel-ошибки prepare-фазы destroy (резолв снапшота сервиса ДО транзакции
// Destroy). Отделены от tx-уровневого [ErrIncarnationNotDestroyable]:
//   - ErrServiceNotRegistered  → caller маппит в internal-error / 500 (сервис
//     incarnation должен быть в реестре сервисов; переиспользуется из
//     upgrade_prepare.go — та же причина).
//   - ErrDestroyScenarioMissing → 422 / 409 validation-failed (force=false и в
//     снапшоте нет scenario `destroy`: teardown выполнить нечем). Возвращается
//     ДО перехода в destroying — incarnation остаётся нетронутой.
//   - ErrLoadTargetSnapshot     → internal-error / 500 (git/loader-фейл;
//     переиспользуется из upgrade_prepare.go).
//   - ErrTargetSnapshotInvalid  → internal-error / 500 (снапшот без манифеста;
//     переиспользуется из upgrade_prepare.go).
//
// ErrServiceNotRegistered / ErrLoadTargetSnapshot / ErrTargetSnapshotInvalid
// объявлены в upgrade_prepare.go — переиспользуем как есть (та же семантика
// резолва снапшота, дубликат был бы багом).
var ErrDestroyScenarioMissing = errors.New("incarnation: service snapshot has no `destroy` scenario")

// destroyScenarioName — имя teardown-сценария в снапшоте сервиса
// (scenario/orchestration.md §1: `scenario/<name>/main.yml`). Совпадает с
// destroyScenarioLabel (destroy.go) по значению, но это разные роли: label —
// метка перехода в state_history, name — имя файла сценария в репо. Держим
// отдельную константу, чтобы будущая смена одной не утянула другую молча.
const destroyScenarioName = "destroy"

// destroyScenarioMainFile — относительный путь точки входа teardown-сценария в
// снапшоте сервиса. Тот же формат, что scenario.scenarioMainFile.
const destroyScenarioMainFile = "scenario/" + destroyScenarioName + "/main.yml"

// DestroyScenarioReader — узкая поверхность загрузчика снапшота сервиса для
// pre-check наличия teardown-сценария: материализует снапшот по ref-у и читает
// из него файл. Реальный [artifact.ServiceLoader] удовлетворяет структурно;
// unit-тесты дают fake. Резолв ref → git-координаты делает [ServiceResolver]
// (общий с upgrade_prepare.go).
type DestroyScenarioReader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// PrepareDestroy — handler-уровневая подготовка destroy (паттерн [PrepareUpgrade]):
// ДО транзакции [Destroy] проверяет, что teardown выполним. Git-загрузка снапшота
// под FOR UPDATE недопустима (долгая I/O под row-lock-ом), поэтому проверка
// вынесена сюда, ПЕРЕД переходом в destroying.
//
// Шаги:
//  1. Resolve(inc.Service) → git-координаты текущей версии сервиса. Ref
//     переопределяется на inc.ServiceVersion (teardown катится той версией
//     сервиса, что развёрнута, а не tip-ом).
//  2. Load(ref) → снапшот сервиса.
//  3. Наличие scenario/destroy/main.yml в снапшоте.
//
// Семантика force:
//   - force=false И scenario `destroy` отсутствует → [ErrDestroyScenarioMissing]
//     (teardown выполнить нечем — caller вернёт 422/409 ДО destroying).
//   - force=true → snapshot всё равно загружается и проверяется (диагностика),
//     но отсутствие scenario НЕ блокирует: force означает «снести без teardown»
//     (S-D3 удалит строку напрямую). ok=true.
//   - scenario присутствует → ok=true независимо от force.
//
// Возвращает загруженный снапшот сервиса (`art`): caller читает из него
// `lifecycle.auto_destroy` (S3 enforcement) без второго git-load-а. При ошибке
// art == nil. art != nil ⇒ art.Manifest валиден (Load парсит manifest).
//
// Чистая от HTTP/MCP-транспорта: возвращает типизированные sentinel-ошибки,
// caller маппит их в свой error-формат. Саму проводку в handler делает S-D4.
// inc приходит уже загруженным (caller делает SelectByName сам — для
// 404-семантики и FOR UPDATE-гонки в Destroy).
func PrepareDestroy(
	ctx context.Context,
	resolver ServiceResolver,
	reader DestroyScenarioReader,
	inc *Incarnation,
	force bool,
) (*artifact.ServiceArtifact, error) {
	ref, ok := resolver.Resolve(inc.Service)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrServiceNotRegistered, inc.Service)
	}
	// Teardown катится развёрнутой версией сервиса, а не tip-ом ветки: scenario
	// `destroy` берём из того же ref-а, что и текущий incarnation.
	ref.Ref = inc.ServiceVersion

	art, err := reader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	if art == nil {
		return nil, ErrTargetSnapshotInvalid
	}

	hasScenario, err := destroyScenarioExists(reader, art)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadTargetSnapshot, err)
	}
	if !hasScenario && !force {
		return nil, fmt.Errorf("%w: %s", ErrDestroyScenarioMissing, inc.Service)
	}
	return art, nil
}

// HasDestroyScenario — экспортированный probe наличия scenario `destroy` в уже
// загруженном снапшоте (S3: handler/MCP перепроверяют teardown-выполнимость
// ПОСЛЕ резолва `lifecycle.auto_destroy`, когда effectiveForce=false — обычный
// teardown-путь). Без второго git-load-а: art уже материализован [PrepareDestroy].
// Тонкая обёртка над [destroyScenarioExists] (та же os.ErrNotExist→false-семантика).
func HasDestroyScenario(reader DestroyScenarioReader, art *artifact.ServiceArtifact) (bool, error) {
	return destroyScenarioExists(reader, art)
}

// destroyScenarioExists проверяет наличие scenario/destroy/main.yml в снапшоте.
// Отсутствие файла (os.ErrNotExist сквозь loader-обёртку) — это (false, nil),
// а не ошибка: «нет teardown-сценария» — нормальный исход проверки, решение по
// нему принимает PrepareDestroy с учётом force. Любая иная I/O-ошибка чтения
// прокидывается caller-у.
func destroyScenarioExists(reader DestroyScenarioReader, art *artifact.ServiceArtifact) (bool, error) {
	_, err := reader.ReadFile(art, destroyScenarioMainFile)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
