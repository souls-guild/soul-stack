package herald

// Resolution of Herald channel configuration and its signing-secret for delivery
// (ADR-052(a)/(e)). Herald record is resolved by name from `heralds` registry on
// each delivery (config could have changed after job queued);
// secret_ref (if set) is resolved from Vault — signing-token is NOT stored
// cleartext in PG (pattern omens.auth_ref).

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// HeraldReader is narrow surface of heralds registry, needed by worker: resolution
// of channel by name at delivery time. Real implementation is closure over
// [SelectHeraldByName]; narrow interface allows fake in unit tests without PG.
type HeraldReader interface {
	HeraldByName(ctx context.Context, name string) (*Herald, error)
}

// KVReader is narrow surface of Vault reading for signing-token resolution (same
// ReadKV as augur broker / render pipeline). *vault.Client satisfies it.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// webhookTarget is resolved delivery parameters of one webhook.
type webhookTarget struct {
	url          string
	headers      map[string]string
	httpAllowed  bool
	allowPrivate bool
	// signingKey is resolved signing-token from Vault (nil → signature not applied).
	signingKey []byte
}

// resolveWebhookTarget extracts delivery parameters of webhook channel from Herald
// record: url, headers, opt-out flags and (if secret_ref is set) signing-token from
// Vault. Error — channel is not webhook / broken config / Vault failure (caller treats
// as terminal-fail of this job delivery, secret does not leak into error text).
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

// configHeaders extracts optional webhook headers from config.headers (map of strings).
// Non-string values are discarded (defensive: JSONB could come from manual edit).
// nil → empty map.
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

// resolveSigningKey reads signing-token of webhook channel from Vault by secret_ref.
// Thin wrapper over [resolveVaultString] (common resolution of single vault-ref
// field), returns raw key bytes for HMAC signature.
func resolveSigningKey(ctx context.Context, kv KVReader, secretRef string) ([]byte, error) {
	s, err := resolveVaultString(ctx, kv, secretRef)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// resolveVaultString reads single string value of secret from Vault by
// vault-ref — common resolver for all secret fields of channels (webhook secret_ref,
// telegram bot_token_ref, slack/mattermost/discord webhook_url_ref, custom
// header_secret_ref, email password_ref).
//
// Ref format: `vault:<mount>/<path>` with optional suffix `#<field>` (symmetry
// vault()/readVaultRef). Field selection:
//   - `#field` is set → use exactly it;
//   - `#field` is omitted AND secret has exactly one field → use it (convenient
//     default for single-key secret);
//   - `#field` is omitted AND multiple fields → error (ambiguous; operator must
//     specify `#field`).
//
// SECURITY: secret value does NOT go into error text; ref is masked by caller
// via MaskSecrets when logging error message.
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
