package handlers

// Reveal-секретов инкарнации (NIM-74): оператор из State-вьюхи раскрывает
// plaintext секрета, объявленного `revealable_secrets` сервиса, под правом
// incarnation.view-secrets. Механизм generic (не redis-хардкод): сервис
// декларирует раскрываемые секреты (id/label/enumerate/vault_ref) в манифесте.
//
// Инварианты безопасности (BLOCKER):
//   - значение секрета покидает домен ТОЛЬКО телом HTTP-ответа — никогда в
//     лог/audit/OTel/текст ошибки (self-audit пишет {name,secret_id,key,path},
//     БЕЗ значения — ADR-064 b);
//   - `key` валидируется паттерном И обязан ∈ enumerate-массива ТЕКУЩЕГО state
//     ДО подстановки в путь (анти-произвол); vault.ParseRef — второй слой
//     (traversal `..`/`.`);
//   - версия манифеста — ВСЕГДА inc.ServiceVersion (parity secretSchemaForIncarnation):
//     клиент версию не задаёт (анти version-craft).

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// reRevealIdent — input-форма secret_id / key (lowercase + `-`/`_`, redis
// AclUser.name-класс). Существование сверяется отдельно (манифест / state);
// невалидная форма → 422 (garbage не доходит до Vault-пути).
var reRevealIdent = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// reRevealServiceSeg — безопасный сегмент Vault-пути для inc.Service перед
// подстановкой в {service} (без `/`,`#`,`..`; kebab). Сервис валиден по reServiceName
// на регистрации — это fail-closed defense-in-depth против инъекции в путь.
var reRevealServiceSeg = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// RevealSecretView — доменная проекция 200-тела POST .../secrets/reveal. Value —
// plaintext секрета. ЕДИНСТВЕННОЕ место выхода значения из домена — тело ответа.
type RevealSecretView struct {
	Value string
}

// RevealableSecretItem — одна декларация в discovery-ответе GET .../secrets/revealable.
type RevealableSecretItem struct {
	SecretID  string
	Label     string
	StatePath string
	Keys      []string
}

// RevealableSecretsView — доменная проекция 200-тела GET .../secrets/revealable.
type RevealableSecretsView struct {
	Items []RevealableSecretItem
}

// RevealSecretTyped — доменная функция POST /v1/incarnations/{name}/secrets/reveal
// (SELF-AUDIT incarnation.secret_revealed). Резолвит plaintext секрета secretID для
// элемента key из Vault. Ошибки — *problemError (422 форма / 404 вне scope | нет
// secretID | key не в state | floor | нет значения в Vault / 500 сбой).
//
// Аудируются ВСЕ ветки после RBAC-гейта (паритет auditInputVault): успех
// result:"ok", denied — result:"denied"+reason (значение НЕ кладём). RBAC-403 —
// gate-уровень (handler не запускается), здесь не аудируется. Форм-422 (битый
// name/secret_id/key) — malformed request ДО резолва инкарнации, не denied-reveal.
func (h *IncarnationHandler) RevealSecretTyped(ctx context.Context, claims *jwt.Claims, name, secretID, key string) (RevealSecretView, error) {
	var zero RevealSecretView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !reRevealIdent.MatchString(secretID) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'secret_id' must match "+reRevealIdent.String())
	}
	if !reRevealIdent.MatchString(key) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'key' must match "+reRevealIdent.String())
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			// Ресурса нет — не аудируем (не denied-reveal, нечего атрибутировать).
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.reveal-secret: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	inScope := h.GetInScopeFor(claims, "view-secrets")
	if inScope == nil || !inScope(inc) {
		// Вне scope — 404 (parity Get: не палим существование чужой инкарнации).
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "out_of_scope", "")
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}

	// Материализация манифеста на inc.ServiceVersion + поиск secretID. Best-effort
	// недоступности снапшота → 404 (секрет нераскрываем), не 500.
	rs, ok := h.revealableSecretByID(ctx, inc, secretID)
	if !ok {
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "unknown_secret_id", "")
		return zero, revealNotFound(secretID, name)
	}

	// key обязан ∈ enumerate-массива ТЕКУЩЕГО state (анти-произвол: нельзя
	// раскрыть путь, которого нет в state сейчас).
	if !containsString(enumerateStateKeys(inc.State, rs.Enumerate), key) {
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "key_not_in_state", "")
		return zero, incProblem(problem.TypeNotFound,
			"key "+key+" is not present in "+statePathTail(rs.Enumerate)+" of incarnation "+name)
	}

	if h.vault == nil {
		return zero, revealNotFound(secretID, name)
	}

	// inc.Service — сегмент Vault-пути; валидируем ПЕРЕД подстановкой (анти-инъекция:
	// без `/`,`#`,`..`). Сервис валиден по reServiceName на регистрации; здесь
	// fail-closed defense-in-depth (data-аномалия → 404, не читаем).
	if !reRevealServiceSeg.MatchString(inc.Service) {
		h.logger.Warn("incarnation.reveal-secret: incarnation service unsafe for vault path",
			slog.String("name", name), slog.String("service", inc.Service))
		return zero, revealNotFound(secretID, name)
	}

	// Литеральная подстановка провалидированных величин (inc.Service — reRevealServiceSeg,
	// inc.Name — NamePattern, key — reRevealIdent И ∈ state) + vault.ParseRef 2-м слоем.
	rendered := strings.ReplaceAll(rs.VaultRef, "{service}", inc.Service)
	rendered = strings.ReplaceAll(rendered, "{incarnation}", inc.Name)
	rendered = strings.ReplaceAll(rendered, "{key}", key)

	body, field := rendered, ""
	if i := strings.LastIndexByte(rendered, '#'); i >= 0 {
		body, field = rendered[:i], rendered[i+1:]
	}
	logical, perr := vault.ParseRef("vault:" + body)
	if perr != nil {
		// Traversal / битая форма пути — секрет нераскрываем (не палим детали пути).
		h.logger.Error("incarnation.reveal-secret: vault ref invalid",
			slog.String("name", name), slog.String("secret_id", secretID), slog.Any("error", perr))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "ref_invalid", "")
		return zero, revealNotFound(secretID, name)
	}

	// ★ Позитивный allowlist (NIM-74 C1, ГЛАВНЫЙ guard): reveal читает ТОЛЬКО под
	// неймспейсом секретов своей инкарнации своего сервиса. Trailing `/` ОБЯЗАТЕЛЕН
	// (иначе prefix-confusion: `redis-prod` матчил бы `redis-prod-other`). ПОСЛЕ
	// ParseRef, ПЕРЕД ReadKV.
	allowedPrefix := "secret/" + inc.Service + "/" + inc.Name + "/"
	if !strings.HasPrefix(logical, allowedPrefix) {
		h.logger.Warn("incarnation.reveal-secret: path outside service/incarnation namespace",
			slog.String("name", name), slog.String("secret_id", secretID), slog.String("path", logical))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "out_of_service_scope", logical)
		return zero, revealNotFound(secretID, name)
	}

	// FLOOR backstop (NIM-74 C1, паритет scenario/input_vault.go §3): страховка для
	// edge-case сервиса с зарезервированным именем (secret/keeper/, secret/internal/).
	// Безусловно ПЕРЕД ReadKV.
	if config.DeniedByVaultFloor(logical, nil) {
		h.logger.Warn("incarnation.reveal-secret: vault floor denied",
			slog.String("name", name), slog.String("secret_id", secretID), slog.String("path", logical))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "floor_denied", logical)
		return zero, revealNotFound(secretID, name)
	}

	data, rerr := h.vault.ReadKV(ctx, logical)
	if rerr != nil {
		if errors.Is(rerr, vault.ErrVaultKVNotFound) {
			h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "vault_miss", logical)
			return zero, incProblem(problem.TypeNotFound, "secret value not found in Vault")
		}
		h.logger.Error("incarnation.reveal-secret: vault read failed",
			slog.String("name", name), slog.String("secret_id", secretID),
			slog.String("path", logical), slog.Any("error", rerr))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "read_error", logical)
		return zero, incProblem(problem.TypeInternalError, "read secret failed")
	}
	value, ok := selectRevealField(data, field)
	if !ok {
		// Нет поля / nopass / не строковое значение — раскрывать нечего.
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "field_missing", logical)
		return zero, incProblem(problem.TypeNotFound, "secret value not found in Vault")
	}

	// SELF-AUDIT успеха (после ReadKV): факт просмотра БЕЗ значения (ADR-064 b).
	h.auditReveal(ctx, claims.Subject, name, secretID, key, "ok", "", logical)

	return RevealSecretView{Value: value}, nil
}

// auditReveal пишет событие incarnation.secret_revealed (успех result:"ok" или
// denied result:"denied"+reason). ЗНАЧЕНИЕ секрета в payload НИКОГДА не кладём
// (ADR-064 b); path — logical Vault-путь (локация, не секрет) при наличии. audit-
// фейл НЕ валит reveal (parity auditInputVault) — warn. h.auditW nil → no-op.
func (h *IncarnationHandler) auditReveal(ctx context.Context, aid, name, secretID, key, result, reason, path string) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"name":      name,
		"secret_id": secretID,
		"key":       key,
		"result":    result,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if path != "" {
		payload["path"] = path
	}
	if err := h.auditW.Write(ctx, &audit.Event{
		EventType: audit.EventIncarnationSecretRevealed,
		Source:    apimiddleware.ScenarioInvocationSource(ctx),
		ArchonAID: aid,
		Payload:   payload,
	}); err != nil {
		h.logger.Warn("incarnation.reveal-secret: audit write failed",
			slog.String("name", name), slog.String("secret_id", secretID),
			slog.String("result", result), slog.Any("error", err))
	}
}

// RevealableSecretsTyped — доменная функция GET /v1/incarnations/{name}/secrets/
// revealable (READ, БЕЗ audit). Для каждого revealable_secret собирает keys из
// enumerate-массива текущего state. Вне scope → 404 (parity Get). Пустой список — валиден.
func (h *IncarnationHandler) RevealableSecretsTyped(ctx context.Context, claims *jwt.Claims, name string) (RevealableSecretsView, error) {
	zero := RevealableSecretsView{Items: []RevealableSecretItem{}}

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.revealable-secrets: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	inScope := h.GetInScopeFor(claims, "view-secrets")
	if inScope == nil || !inScope(inc) {
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}

	items := make([]RevealableSecretItem, 0)
	for _, rs := range h.revealableSecretsFor(ctx, inc) {
		keys := enumerateStateKeys(inc.State, rs.Enumerate)
		if keys == nil {
			keys = []string{}
		}
		items = append(items, RevealableSecretItem{
			SecretID:  rs.ID,
			Label:     rs.Label,
			StatePath: statePathTail(rs.Enumerate),
			Keys:      keys,
		})
	}
	return RevealableSecretsView{Items: items}, nil
}

// revealableSecretByID материализует манифест инкарнации и ищет декларацию по id.
func (h *IncarnationHandler) revealableSecretByID(ctx context.Context, inc *incarnation.Incarnation, secretID string) (config.RevealableSecret, bool) {
	for _, rs := range h.revealableSecretsFor(ctx, inc) {
		if rs.ID == secretID {
			return rs, true
		}
	}
	return config.RevealableSecret{}, false
}

// revealableSecretsFor материализует снапшот сервиса на версии инкарнации
// (inc.ServiceVersion — та же авторитетная версия, что secretSchemaForIncarnation) и
// возвращает `revealable_secrets` манифеста. Best-effort: loader/services nil,
// сервис не зарегистрирован, ошибка загрузки → nil (нечего раскрывать).
func (h *IncarnationHandler) revealableSecretsFor(ctx context.Context, inc *incarnation.Incarnation) []config.RevealableSecret {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	if inc.ServiceVersion != "" {
		ref.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, ref)
	if err != nil || art == nil || art.Manifest == nil {
		return nil
	}
	return art.Manifest.RevealableSecrets
}

// enumerateStateKeys резолвит enumerate-массив из state (`state.<array>`) и собирает
// имена элементов (`element.name` — конвенция redis AclUser.name). Непокрытый путь /
// не-массив → nil (fail-closed, без паники). Ключи фильтруются тем же reRevealIdent,
// что валидирует reveal (discovery не рекламирует ключ, который reveal отобьёт 422),
// и дедупятся (дубль-имя в state не плодит дублей в discovery/множестве проверки).
func enumerateStateKeys(state map[string]any, enumerate string) []string {
	v, ok := resolveStatePath(state, enumerate)
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(arr))
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		nm, ok := m["name"].(string)
		if !ok || nm == "" || !reRevealIdent.MatchString(nm) {
			continue
		}
		if _, dup := seen[nm]; dup {
			continue
		}
		seen[nm] = struct{}{}
		out = append(out, nm)
	}
	return out
}

// selectRevealField выбирает одно строковое поле секрета (форма `#field`
// обязательна). Пустое поле / отсутствие / не-строка → (”", false): раскрываем
// только скалярное значение, структуру не сериализуем.
func selectRevealField(data map[string]any, field string) (string, bool) {
	if field == "" {
		return "", false
	}
	v, ok := data[field]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// revealNotFound — унифицированный 404 «секрет нераскрываем» (нет secretID /
// снапшот недоступен / битый ref / vault не сконфигурирован): один текст, чтобы не
// различать причины наружу.
func revealNotFound(secretID, name string) error {
	return incProblem(problem.TypeNotFound, "secret "+secretID+" is not revealable for incarnation "+name)
}

// containsString — линейный поиск (наборы key малы: единицы-десятки ACL-юзеров).
func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
