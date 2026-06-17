package scenario

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ErrInputInvalid — sync-валидация переданного оператором input против
// scenario `input:`-схемы провалена (required-поле без default не передано,
// type-mismatch, нарушение pattern/enum/length). HTTP-handler маппит в 422
// `input_invalid`; MCP — в аналогичную ошибку.
//
// Историческая дыра (root-cause бага «создал инкарнацию без обязательных
// полей»): value-валидация input жила ТОЛЬКО в async run-goroutine (run.go
// шаг 4.5, ResolveInputValuesVault → abort→error_locked). POST /v1/incarnations
// и POST .../scenarios/{scenario} возвращали 202 «принято» ДО валидации, а
// реальный отказ оседал в incarnation.status=error_locked постфактум. Этот
// sentinel + [ValidateInput] закрывают дыру: проверка идёт sync ДО мутации.
var ErrInputInvalid = errors.New("scenario: input invalid")

// InputScenarioLoader — узкая поверхность [artifact.ServiceLoader], нужная
// [ValidateInput]: материализовать снапшот service-ref-а и прочитать
// scenario/<name>/main.yml. *artifact.ServiceLoader удовлетворяет; unit-тесты
// подставляют fake без git-стека.
type InputScenarioLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// ValidateInput синхронно проверяет переданный оператором input против
// `input:`-схемы scenario `scenarioName` сервиса `ref` — ДО любой мутации
// (insert incarnation / enqueue прогона). Зеркало фазы «merge дефолтов +
// required + value-валидация» из async run-goroutine (run.go шаг 4.5), но
// вынесенное в request-путь, чтобы оператор получил 422 сразу, а не
// «создано → error_locked» постфактум.
//
// Vault-ref-резолв ЗДЕСЬ НЕ выполняется (config.ResolveInputValues, не
// ...Vault): значение `vault:...` secret-поля проходит как строка. Required +
// type + pattern/enum/length проверяются полностью; финальный scoped vault-
// резолв остаётся в run-goroutine (инвариант A ADR-027 — секрет не материализуем
// в request-пути). Это адекватно: required-дыра — про отсутствие поля, а не про
// содержимое секрета.
//
// scn.Input парсится напрямую из top-level `input:` блока main.yml — include-
// раскрытие не требуется (include приносит tasks, не input-схему). Ошибки
// загрузки/парсинга снапшота возвращаются как есть (handler → 500/502); ошибка
// собственно value-валидации оборачивается в [ErrInputInvalid] (handler → 422).
func ValidateInput(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName string, provided map[string]any) error {
	if loader == nil {
		// Без loader-а sync-валидация невозможна — НЕ молча пропускаем (это и
		// была дыра). Возвращаем явную ошибку конфигурации; handler решит
		// (в проде loader всегда сконфигурирован вместе с runner-ом).
		return fmt.Errorf("scenario: validate input: loader is not configured")
	}

	art, err := loader.Load(ctx, ref)
	if err != nil {
		return fmt.Errorf("scenario: validate input: load service: %w", err)
	}

	rel := fmt.Sprintf(scenarioMainFile, scenarioName)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return fmt.Errorf("scenario: validate input: read %s: %w", rel, err)
	}
	scn, _, diags, err := config.LoadScenarioManifestFromBytes(rel, data, config.ValidateOptions{})
	if err != nil {
		return fmt.Errorf("scenario: validate input: parse %s: %w", rel, err)
	}
	if diag.HasErrors(diags) {
		return fmt.Errorf("scenario: validate input: %s невалиден: %s", rel, firstError(diags))
	}

	// merge дефолтов + required + value-валидация (type/enum/pattern/length,
	// рекурсивно вглубь array/object). vault-ref не резолвится (string-pass).
	if _, verr := config.ResolveInputValues(scn.Input, provided); verr != nil {
		return fmt.Errorf("%w: %v", ErrInputInvalid, verr)
	}
	return nil
}
