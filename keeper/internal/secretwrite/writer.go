// Package secretwrite материализует plaintext-секреты оператора в Vault по
// детерминированным путям (ADR-064, NIM-11). Dual-mode приёма секрета: оператор
// передаёт значение (plaintext) вместо vault-ref, keeper пишет его в Vault сам и
// хранит в Postgres только внутренний ref. Один write-слой для Herald и Provider
// поверх того же vault.Client.WriteKV, что sigil/cert — обобщение keeper-side
// write-path, не новый инфра-код.
package secretwrite

import (
	"context"
	"fmt"
	"regexp"
)

// Домены секретов — первый сегмент детерминированного пути secret/<domain>/…
const (
	DomainHerald   = "herald"
	DomainProvider = "provider"
)

// defaultMount — KV-mount по умолчанию (совпадает с vault.defaultKVMount).
const defaultMount = "secret"

// segmentRe — безопасный сегмент пути (domain/entity/field): буквы/цифры/`_`/`-`.
// Отсекает `.`/`..`/слеши/пустое — исключает обход scope в Vault-пути (ParseRef
// тоже реджектит `..`, здесь fail-closed на входе write-path).
var segmentRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// VaultWriter — узкая поверхность записи Vault KV (реализуется
// vault.Client.WriteKV). Сужение до интерфейса даёт fake в unit/guard-тестах без
// реального Vault (симметрично sigil.VaultWriter).
type VaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// Writer пишет plaintext-секрет оператора в Vault по пути
// <mount>/<domain>/<entity>/<field> и возвращает внутренний vault-ref для PG.
// mount — из keeper.yml (vault.kv_mount, default "secret"); тот же клиент, что
// sigil/cert. БЕЗОПАСНОСТЬ: значение секрета в текст ошибок/логов НЕ попадает.
type Writer struct {
	vault VaultWriter
	mount string
}

// NewWriter собирает writer. v обязателен (nil → ошибка). mount=="" → "secret".
func NewWriter(v VaultWriter, mount string) (*Writer, error) {
	if v == nil {
		return nil, fmt.Errorf("secretwrite: nil VaultWriter")
	}
	if mount == "" {
		mount = defaultMount
	}
	return &Writer{vault: v, mount: mount}, nil
}

// WriteString пишет одиночное строковое поле секрета {field: value} по
// детерминированному пути и возвращает ref
// vault:<mount>/<domain>/<entity>/<field>#<field>. Явный #field делает резолв
// (resolveVaultString) независимым от числа полей секрета. value в ошибку не
// попадает.
func (w *Writer) WriteString(ctx context.Context, domain, entity, field, value string) (string, error) {
	path, err := w.path(domain, entity, field)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("secretwrite: empty %s/%s value", domain, field)
	}
	if err := w.vault.WriteKV(ctx, path, map[string]any{field: value}); err != nil {
		return "", fmt.Errorf("secretwrite: write %s/%s/%s: %w", domain, entity, field, err)
	}
	return "vault:" + path + "#" + field, nil
}

// WriteMap пишет multi-field секрет (напр. cloud credentials) по
// детерминированному пути и возвращает ref vault:<mount>/<domain>/<entity>/<field>
// (без #field — потребитель читает весь map, как cloud.credentials.Resolve).
// Значения секрета в ошибку не попадают.
func (w *Writer) WriteMap(ctx context.Context, domain, entity, field string, data map[string]any) (string, error) {
	path, err := w.path(domain, entity, field)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("secretwrite: empty %s/%s data", domain, field)
	}
	if err := w.vault.WriteKV(ctx, path, data); err != nil {
		return "", fmt.Errorf("secretwrite: write %s/%s/%s: %w", domain, entity, field, err)
	}
	return "vault:" + path, nil
}

// path собирает и валидирует детерминированный logical-путь
// <mount>/<domain>/<entity>/<field>. domain/entity/field обязаны быть
// безопасными сегментами (segmentRe).
func (w *Writer) path(domain, entity, field string) (string, error) {
	if !segmentRe.MatchString(domain) {
		return "", fmt.Errorf("secretwrite: invalid domain %q", domain)
	}
	if !segmentRe.MatchString(entity) {
		return "", fmt.Errorf("secretwrite: invalid entity %q", entity)
	}
	if !segmentRe.MatchString(field) {
		return "", fmt.Errorf("secretwrite: invalid field %q", field)
	}
	return w.mount + "/" + domain + "/" + entity + "/" + field, nil
}
