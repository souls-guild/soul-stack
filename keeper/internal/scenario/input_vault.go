package scenario

// Scoped-резолв `vault:`-ref в operator-input (docs/input.md → «vault_scope»).
//
// Это ОТДЕЛЬНЫЙ от авторского (`vault:`/`${vault()}` в task params) ограниченный
// канал. Авторский канал — доверенный (автор сервиса), резолвится render-фазой
// (keeper/internal/render). Input-канал — оператор-ввод, поэтому резолв скоупится
// по `vault_scope` поля + hard deny-list (форк C), а каждый резолв (ok/denied)
// аудируется (security-сигнал).
//
// Логика проверки одного ref: есть vault_scope? нет → reject; путь матчит scope?
// нет → reject; путь в deny-list? да → reject; иначе ReadKV. Проверка scope/deny
// — чистая (shared/config), здесь добавляется чтение KV + audit.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// InputVaultReader — узкое подмножество keeper/internal/vault.Client: чтение KV.
// Тот же общий клиент, что резолвит авторские refs и читает signing-key.
type InputVaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// inputVaultAuditCtx — неизменный контекст прогона для audit-event-а резолва:
// кто (aid), куда (incarnation/scenario). Поле и путь добавляются на месте.
type inputVaultAuditCtx struct {
	aid         string
	incarnation string
	scenario    string
}

// ошибки резолва input-vault-ref. Сообщения НЕ несут резолвнутый секрет; путь —
// не секрет (логируется), но в operator-facing-ошибку кладём только причину.
var (
	errInputVaultNoScope = errors.New("значение vault:-ref в input запрещено: поле без vault_scope (default-deny)")
	errInputVaultOutOf   = errors.New("значение vault:-ref в input вне разрешённого vault_scope")
	errInputVaultDenied  = errors.New("значение vault:-ref в input ведёт к запрещённому пути (deny-list)")
)

// newInputVaultResolver собирает config.InputVaultResolver для одного прогона.
// resolve вызывается из config.ResolveInputValuesVault на каждом string+`vault:`
// значении. vc может быть nil — тогда фабрика возвращает nil (input-vault-refs
// не поддержаны; default-deny отработает раньше в ResolveInputValues, который
// не резолвит refs). extraDeny — config-расширение system-floor.
func (r *Runner) newInputVaultResolver(ctx context.Context, ac inputVaultAuditCtx, extraDeny []string) config.InputVaultResolver {
	return buildInputVaultResolver(ctx, r.deps.Vault, r.deps.Audit, r.logger, ac, extraDeny)
}

// buildInputVaultResolver — package-level форма [Runner.newInputVaultResolver]:
// собирает config.InputVaultResolver из явных vault-reader / audit-writer /
// logger. Вынесена, чтобы Acolyte-путь ([RenderForHost]) воспроизводил тот же
// scoped-резолв input-vault-ref без подъёма Runner-а. Поведение идентично
// старому методу (метод теперь делегирует сюда). vc == nil → nil (refs не
// резолвятся).
func buildInputVaultResolver(ctx context.Context, vc InputVaultReader, w audit.Writer, log *slog.Logger, ac inputVaultAuditCtx, extraDeny []string) config.InputVaultResolver {
	if vc == nil {
		return nil
	}
	return func(name string, s *config.InputSchema, raw string) (any, error) {
		// 1. поле без vault_scope, но со значением vault: → default-deny.
		if s.VaultScope == "" {
			auditInputVault(ctx, w, ac, name, "", "denied", "no_scope", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultNoScope)
		}

		// Разбор ref в logical-path (без vault:-префикса и #field-суффикса).
		logical, field, perr := parseInputVaultRef(raw)
		if perr != nil {
			auditInputVault(ctx, w, ac, name, "", "denied", "parse_error", log)
			return nil, fmt.Errorf("input %q: %w", name, perr)
		}

		// 2. scope-match.
		if !config.MatchesVaultScope(s.VaultScope, logical) {
			auditInputVault(ctx, w, ac, name, logical, "denied", "out_of_scope", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultOutOf)
		}

		// 3. hard deny-list (ПОСЛЕ scope, безусловно: страховка от ошибки
		//    автора в vault_scope).
		if config.DeniedByVaultFloor(logical, extraDeny) {
			auditInputVault(ctx, w, ac, name, logical, "denied", "deny_list", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultDenied)
		}

		// 4. ReadKV.
		data, err := vc.ReadKV(ctx, logical)
		if err != nil {
			auditInputVault(ctx, w, ac, name, logical, "denied", "read_error", log)
			// Ошибку Vault пробрасываем без значения секрета (ReadKV их и не
			// несёт — только путь/sentinel).
			return nil, fmt.Errorf("input %q: чтение vault-ref: %w", name, err)
		}

		val, err := selectVaultField(data, field)
		if err != nil {
			auditInputVault(ctx, w, ac, name, logical, "denied", "field_missing", log)
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		auditInputVault(ctx, w, ac, name, logical, "ok", "", log)
		return val, nil
	}
}

// parseInputVaultRef разбирает `vault:<mount>/<path>[#<field>]` в logical-path и
// опц. field. Та же форма, что у авторских refs (render.readVaultRef), но без
// импорта render-пакета — input-канал самостоятелен. `${…}`-маркер запрещён
// (статическая строка, как в авторском канале).
func parseInputVaultRef(ref string) (logical, field string, err error) {
	body := ref
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return "", "", errors.New("vault-ref: пустое имя поля после '#'")
		}
	}
	logical, perr := vault.ParseRef(body)
	if perr != nil {
		return "", "", perr
	}
	return logical, field, nil
}

// selectVaultField извлекает поле из KV-секрета. Без field — отдаём весь map
// (downstream-валидация ожидает string для secret-поля, но решение «весь map»
// оставляем потребителю: на практике secret-поля используют #field).
func selectVaultField(data map[string]any, field string) (any, error) {
	if field == "" {
		return data, nil
	}
	v, ok := data[field]
	if !ok {
		return nil, fmt.Errorf("поле %q отсутствует в секрете", field)
	}
	return v, nil
}

// auditInputVault пишет audit-event резолва input-vault-ref. path — НЕ секрет
// (логируется), значение секрета НЕ кладётся. denied тоже аудируется
// (security-сигнал). aid инициатора — в payload (write-path keeper-internal,
// archon_aid колонка = NULL по source-таксономии ADR-022). audit-фейл не валит
// прогон, только логируется: блокировать резолв из-за audit-write нельзя, но и
// молча терять security-trail нельзя. w == nil → trail не пишется (unit/L0).
func auditInputVault(ctx context.Context, w audit.Writer, ac inputVaultAuditCtx, field, path, result, reason string, log *slog.Logger) {
	if w == nil {
		return
	}
	payload := map[string]any{
		"field":       field,
		"incarnation": ac.incarnation,
		"scenario":    ac.scenario,
		"result":      result,
	}
	if ac.aid != "" {
		payload["aid"] = ac.aid
	}
	if path != "" {
		payload["path"] = path
	}
	if reason != "" {
		payload["reason"] = reason
	}
	ev := &audit.Event{
		EventType: audit.EventInputVaultResolved,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := w.Write(ctx, ev); err != nil && log != nil {
		log.Warn("scenario: запись audit input.vault_resolved провалена",
			slog.String("field", field), slog.String("result", result), slog.Any("error", err))
	}
}
