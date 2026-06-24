package audit

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// SealHooks — process-global observability-точки слоя regex-last-resort
// ([ADR-010] §7.4, слой 4). Декуплинг: shared/audit не тянет prometheus —
// keeper подключает метрику keeper_mask_regex_fallback_total + warn-логгер один
// раз на старте (cmd/keeper), сеттером ниже. nil-поля → no-op (тесты/офлайн/Soul).
type SealHooks struct {
	// RegexFallback инкрементит метрику regex-fallback (keeper_mask_regex_fallback_total).
	RegexFallback func(path string)
	// Logger — канал warn-лога regex-fallback (путь ячейки, без значения).
	Logger *slog.Logger
}

// DefaultSealHooks — глобальные хуки, которые зовёт [MaskSecretsWithSchema].
// Zero-value (оба nil) — no-op. Устанавливается [SetSealHooks] в cmd/keeper.
var DefaultSealHooks SealHooks

// SetSealHooks подключает process-global observability слоя regex-last-resort.
// Вызывается один раз на старте keeper (после регистрации метрик). Идемпотентно
// перезаписывает. shared/audit-сторона: prometheus-зависимость остаётся в keeper.
func SetSealHooks(h SealHooks) { DefaultSealHooks = h }

// reIdx матчит идексный сегмент пути `[<digits>]` для обобщения к `[]` —
// схема/seal-набор описывают элемент массива без конкретного индекса
// (`acl[].password`), а путь ячейки несёт конкретный (`acl[0].password`).
var reIdx = regexp.MustCompile(`\[\d+\]`)

// normalizeIdx заменяет все конкретные индексы пути на `[]`: `acl[0].users[2].pw`
// → `acl[].users[].pw`. Так SecretPathSet/Sealed могут хранить обобщённую
// idx-форму (одна запись на элемент массива), а сверка ловит любой индекс.
func normalizeIdx(path string) string {
	if !strings.ContainsRune(path, '[') {
		return path
	}
	return reIdx.ReplaceAllString(path, "[]")
}

// joinPath/joinIdx — построение dot/idx-пути ячейки, БИТ-В-БИТ как
// keeper/internal/render.joinKey/joinIdx (там путь ведётся при рендере; здесь
// сверяется при маскинге — формы обязаны совпадать). joinPath на пустом path
// возвращает key (корневой ключ без ведущей точки).
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIdx(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}
