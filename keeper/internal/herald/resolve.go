package herald

// Резолв конфигурации Herald-канала и его signing-секрета для доставки
// (ADR-052(a)/(e)). Herald-запись резолвится по имени из реестра `heralds` на
// каждую доставку (config мог измениться после постановки job-а в очередь);
// secret_ref (если задан) резолвится из Vault — signing-token в PG cleartext НЕ
// хранится (паттерн omens.auth_ref).

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// HeraldReader — узкая поверхность реестра heralds, нужная worker-у: резолв
// канала по имени на момент доставки. Реальная реализация — замыкание над
// [SelectHeraldByName]; узкий интерфейс даёт fake в unit-тестах без PG.
type HeraldReader interface {
	HeraldByName(ctx context.Context, name string) (*Herald, error)
}

// KVReader — узкая поверхность Vault-чтения для резолва signing-token (тот же
// ReadKV, что у augur-брокера / render-pipeline). *vault.Client удовлетворяет.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// webhookTarget — резолвнутые параметры доставки одного webhook-а.
type webhookTarget struct {
	url          string
	headers      map[string]string
	httpAllowed  bool
	allowPrivate bool
	// signingKey — резолвленный из Vault signing-token (nil → подпись не ставится).
	signingKey []byte
}

// resolveWebhookTarget извлекает параметры доставки webhook-канала из Herald-
// записи: url, headers, opt-out-флаги и (если задан secret_ref) signing-token из
// Vault. Ошибка — канал не webhook / битый config / Vault-сбой (caller трактует
// как terminal-fail доставки этого job-а, секрет в текст ошибки не утекает).
func resolveWebhookTarget(ctx context.Context, h *Herald, kv KVReader) (*webhookTarget, error) {
	if h.Type != HeraldWebhook {
		return nil, fmt.Errorf("herald: channel %q is not webhook (type %q)", h.Name, h.Type)
	}
	rawURL, _ := h.Config["url"].(string)
	if rawURL == "" {
		return nil, fmt.Errorf("herald: channel %q webhook config has no url", h.Name)
	}
	t := &webhookTarget{
		url:          rawURL,
		headers:      configHeaders(h.Config),
		httpAllowed:  configBool(h.Config, "http_allowed"),
		allowPrivate: configBool(h.Config, "allow_private"),
	}
	if h.SecretRef != nil && *h.SecretRef != "" {
		key, err := resolveSigningKey(ctx, kv, *h.SecretRef)
		if err != nil {
			return nil, err
		}
		t.signingKey = key
	}
	return t, nil
}

// configHeaders извлекает опц. webhook-заголовки из config.headers (map строк).
// Не-строковые значения отбрасываются (defensive: JSONB мог прийти из ручной
// правки). nil → пустая map.
func configHeaders(config map[string]any) map[string]string {
	raw, ok := config["headers"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// resolveSigningKey читает signing-token webhook-канала из Vault по secret_ref.
// Тонкая обёртка над [resolveVaultString] (общий резолв одиночного vault-ref-
// поля), возвращает raw-байты ключа для HMAC-подписи.
func resolveSigningKey(ctx context.Context, kv KVReader, secretRef string) ([]byte, error) {
	s, err := resolveVaultString(ctx, kv, secretRef)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// resolveVaultString читает одиночное строковое значение секрета из Vault по
// vault-ref — общий резолвер для всех секрет-полей каналов (webhook secret_ref,
// telegram bot_token_ref, slack/mattermost/discord webhook_url_ref, custom
// header_secret_ref, email password_ref).
//
// Формат ref: `vault:<mount>/<path>` с опц. суффиксом `#<field>` (симметрия
// vault()/readVaultRef). Выбор поля:
//   - `#field` задан → берётся именно оно;
//   - `#field` опущен И в секрете ровно одно поле → берётся оно (удобный
//     дефолт для секрета-на-один-ключ);
//   - `#field` опущен И полей несколько → ошибка (неоднозначно; оператор обязан
//     указать `#field`).
//
// БЕЗОПАСНОСТЬ: значение секрета в текст ошибок НЕ попадает; ref маскируется
// caller-ом через MaskSecrets при логировании error-message.
func resolveVaultString(ctx context.Context, kv KVReader, secretRef string) (string, error) {
	if kv == nil {
		return "", fmt.Errorf("herald: secret ref set but no Vault client configured")
	}
	body := strings.TrimPrefix(secretRef, "vault:")
	pathPart, field, hasField := strings.Cut(body, "#")
	ref := "vault:" + pathPart
	logicalPath, err := vault.ParseRef(ref)
	if err != nil {
		return "", fmt.Errorf("herald: invalid secret ref: %w", err)
	}
	data, err := kv.ReadKV(ctx, logicalPath)
	if err != nil {
		return "", fmt.Errorf("herald: read secret: %w", err)
	}

	var rawVal any
	if hasField {
		if field == "" {
			return "", fmt.Errorf("herald: secret ref has empty #field")
		}
		v, ok := data[field]
		if !ok {
			return "", fmt.Errorf("herald: secret has no field %q", field)
		}
		rawVal = v
	} else {
		if len(data) != 1 {
			return "", fmt.Errorf("herald: secret has %d fields — ref must specify #field", len(data))
		}
		for _, v := range data {
			rawVal = v
		}
	}
	s, ok := rawVal.(string)
	if !ok {
		return "", fmt.Errorf("herald: secret field is not a string")
	}
	return s, nil
}
