package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// PKI-mount-defaults. Match keeper.yml::vault.pki_mount / pki_role
// (см. docs/keeper/config.md → vault). При пустых значениях NewClient
// в M2.1.b не подставляет defaults — caller (keeper run) обязан
// передать резолвенные значения; константы оставлены для readiness-
// проверок / документации.
const (
	DefaultPKIMount = "pki"
	DefaultPKIRole  = "soul-seed"
)

// SignedCertificate — результат подписания CSR через Vault PKI sign-RPC.
//
// CertificatePEM содержит ровно один CERTIFICATE-PEM-блок (issued cert).
// CAChainPEM — конкатенация PEM-блоков `issuing_ca` + `ca_chain[]`
// (если backend выдаёт цепочку), в том порядке, в каком его отдаёт
// Vault. Не валидирует — caller передаёт клиенту as-is.
type SignedCertificate struct {
	CertificatePEM []byte
	CAChainPEM     []byte
	SerialNumber   string
	NotAfter       time.Time
}

// Sentinel-ошибки PKI-flow.
var (
	// ErrPKIMountEmpty — mount пуст. Caller (gRPC handler) маппит в
	// internal error (config-misconfig).
	ErrPKIMountEmpty = errors.New("vault pki: mount is empty")
	// ErrPKIRoleEmpty — role пуст.
	ErrPKIRoleEmpty = errors.New("vault pki: role is empty")
	// ErrPKICSREmpty — пустой CSR PEM (валидация ДО round-trip-а в Vault).
	ErrPKICSREmpty = errors.New("vault pki: csr is empty")
	// ErrPKIResponseInvalid — Vault вернул успешный ответ, но без
	// полей `certificate` / `serial_number`. Кричит «PKI backend
	// сконфигурирован странно»; caller — 500.
	ErrPKIResponseInvalid = errors.New("vault pki: response missing required fields")
)

// SignCSR подписывает PEM-CSR через Vault PKI `<mount>/sign/<role>`.
//
// Не вычисляет fingerprint и не пишет в реестр — это ответственность
// caller-а (gRPC Bootstrap-handler).
//
// Параметры:
//   - mount — путь mount-а PKI engine (без trailing slash). Pass
//     `cfg.Vault.PKIMount` из keeper.yml.
//   - role — имя PKI role, под которой Vault выдаёт SoulSeed-сертификаты.
//     Pass `cfg.Vault.PKIRole`.
//   - csrPEM — PEM-encoded CSR от Soul-а (BootstrapRequest.csr_pem). Не
//     валидируется здесь — Vault сам отвергает невалидный CSR с 400.
//
// Возврат:
//   - [SignedCertificate] с CertificatePEM + CAChainPEM + SerialNumber + NotAfter.
//   - sentinel ErrPKI* на pre-flight checks.
//   - wrapped fmt.Errorf на транспортные / Vault-ошибки.
func (c *Client) SignCSR(ctx context.Context, mount, role, csrPEM string) (*SignedCertificate, error) {
	if mount == "" {
		return nil, ErrPKIMountEmpty
	}
	if role == "" {
		return nil, ErrPKIRoleEmpty
	}
	if strings.TrimSpace(csrPEM) == "" {
		return nil, ErrPKICSREmpty
	}

	mountClean := strings.TrimSuffix(mount, "/")
	path := mountClean + "/sign/" + role

	resp, err := c.c.Logical().WriteWithContext(ctx, path, map[string]any{
		// `format: pem` — Vault default; явный pass убирает неоднозначность,
		// если backend настроен с `default_format: der`.
		"format": "pem",
		"csr":    csrPEM,
	})
	if err != nil {
		return nil, fmt.Errorf("vault pki: sign %q: %w", path, err)
	}
	if resp == nil || resp.Data == nil {
		return nil, fmt.Errorf("%w: empty response from %s", ErrPKIResponseInvalid, path)
	}

	out := &SignedCertificate{}

	certVal, ok := resp.Data["certificate"].(string)
	if !ok || certVal == "" {
		return nil, fmt.Errorf("%w: missing 'certificate' field", ErrPKIResponseInvalid)
	}
	out.CertificatePEM = []byte(certVal)

	if serial, ok := resp.Data["serial_number"].(string); ok && serial != "" {
		out.SerialNumber = serial
	} else {
		return nil, fmt.Errorf("%w: missing 'serial_number' field", ErrPKIResponseInvalid)
	}

	if exp, ok := resp.Data["expiration"]; ok {
		t, err := coerceExpiration(exp)
		if err != nil {
			return nil, fmt.Errorf("vault pki: parse expiration: %w", err)
		}
		out.NotAfter = t
	}

	// Vault PKI sign возвращает `issuing_ca` (single PEM) и `ca_chain`
	// ([]interface{} с PEM-строками). Конкатенируем в одном поле в
	// порядке issuing_ca → ca_chain[…], чтобы Soul мог прямо положить
	// в TrustPool без дополнительной разборки.
	var chain strings.Builder
	if ica, ok := resp.Data["issuing_ca"].(string); ok && ica != "" {
		chain.WriteString(ica)
		if !strings.HasSuffix(ica, "\n") {
			chain.WriteByte('\n')
		}
	}
	if raw, ok := resp.Data["ca_chain"].([]any); ok {
		for _, e := range raw {
			s, ok := e.(string)
			if !ok || s == "" {
				continue
			}
			chain.WriteString(s)
			if !strings.HasSuffix(s, "\n") {
				chain.WriteByte('\n')
			}
		}
	}
	out.CAChainPEM = []byte(chain.String())

	return out, nil
}

// coerceExpiration разбирает `expiration` из Vault PKI sign-response.
//
// Vault отдаёт это поле как **unix timestamp** в JSON-number
// (decoded vaultapi-клиентом как json.Number → string). Поддерживаем
// json.Number, числовые types и string-форму (на случай custom-backend).
func coerceExpiration(v any) (time.Time, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil")
	case int:
		return time.Unix(int64(x), 0).UTC(), nil
	case int64:
		return time.Unix(x, 0).UTC(), nil
	case float64:
		return time.Unix(int64(x), 0).UTC(), nil
	default:
		// json.Number / fmt.Stringer / string — единая ветка через String().
		// vaultapi настраивает json-decoder с UseNumber, поэтому
		// json.Number — основной кейс.
		type numberer interface{ Int64() (int64, error) }
		if n, ok := v.(numberer); ok {
			i, err := n.Int64()
			if err != nil {
				return time.Time{}, fmt.Errorf("json.Number: %w", err)
			}
			return time.Unix(i, 0).UTC(), nil
		}
		if s, ok := v.(string); ok {
			// fallback: RFC3339 для custom-backend-ов.
			t, err := time.Parse(time.RFC3339, s)
			if err == nil {
				return t.UTC(), nil
			}
			return time.Time{}, fmt.Errorf("string %q: %w", s, err)
		}
		return time.Time{}, fmt.Errorf("unsupported type %T", v)
	}
}

// _enforceVaultapiNumber — compile-time assertion, что vaultapi-import
// не выкинули случайным go-mod-tidy: SignCSR использует Logical() из
// vaultapi.Client. Без этой проверки regression «pki.go не зависит от
// vaultapi» не ловится unit-тестами (они mock-аются на уровне Client).
var _ = (*vaultapi.Client)(nil)
