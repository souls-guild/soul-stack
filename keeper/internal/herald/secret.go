package herald

// Dual-mode приём секрета Herald (ADR-064, NIM-11). Оператор передаёт секрет
// значением (plaintext) вместо vault-ref: top-level webhook signing-secret
// (Secret XOR SecretRef) и config-поля канала (<base> XOR <base>_ref для каждого
// Secret-поля дескриптора типа — telegram bot_token, slack/discord webhook_url,
// custom header_secret). Keeper пишет plaintext в Vault по детерминированному
// пути secret/herald/<name>/<field> и заменяет на внутренний ref; plaintext в
// PG/логи/audit/View НЕ попадает.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/secretwrite"
)

// SecretWriter — узкая поверхность материализации plaintext-секрета в Vault
// (реализуется *secretwrite.Writer). nil → dual-mode plaintext недоступен.
type SecretWriter interface {
	WriteString(ctx context.Context, domain, entity, field, value string) (string, error)
}

// ErrPlaintextDisabled — оператор передал plaintext-секрет, но приём выключен
// (ADR-064 митигация a: требуется TLS-фронт Operator API/MCP + secret_ingest.
// accept_plaintext). Заворачивается в [ErrValidation] → 422.
var ErrPlaintextDisabled = errors.New("plaintext secret ingestion disabled (enable secret_ingest.accept_plaintext on a TLS-fronted Operator API, or provide a *_ref)")

// materializeHeraldSecrets переводит plaintext-секреты Herald-записи в Vault по
// детерминированному пути secret/herald/<name>/<field>, заменяя их внутренним
// vault-ref (ADR-064). Обрабатывает top-level Secret и config-поля <base> для
// каждого Secret-поля <base>_ref типа канала. plaintext после записи стирается
// из h (в PG/audit/View не попадает). XOR-инвариант: ровно один из value/ref на
// каждое секрет-поле.
//
// Вызывается Service-ом ДО Insert/Update. plaintext + accept=false (или w=nil)
// → [ErrPlaintextDisabled]. Ошибки не несут значения секрета.
func materializeHeraldSecrets(ctx context.Context, w SecretWriter, accept bool, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	// entity=<name> обязан быть безопасным сегментом пути ДО записи в Vault
	// (materializeField пишет secret/herald/<name>/…). Формат имени тут же
	// проверит validateHerald при Insert, но для write-path нужна проверка ДО.
	if !ValidName(h.Name) {
		return wrapValidation(fmt.Errorf("invalid name %q (must match %s)", h.Name, NamePattern))
	}

	wrote := false

	// --- top-level webhook signing secret (Secret XOR SecretRef) ---
	did, err := materializeField(ctx, w, accept, h.Name, "secret",
		ptrStr(h.Secret), ptrStr(h.SecretRef),
		func(ref string) { h.SecretRef = &ref })
	if err != nil {
		return err
	}
	h.Secret = nil // plaintext стёрт
	wrote = wrote || did

	// --- config-поля канала (<base> XOR <base>_ref для каждого Secret-поля) ---
	fields, ok := fieldsFor(h.Type)
	if ok {
		for _, f := range fields {
			if !f.Secret {
				continue
			}
			base := strings.TrimSuffix(f.Name, "_ref") // bot_token_ref → bot_token
			if base == f.Name {
				continue // не *_ref-поле (защита; все Secret-поля дескриптора — *_ref)
			}
			plainVal, _ := h.Config[base].(string)
			refVal, _ := h.Config[f.Name].(string)
			refField := f.Name
			did, err := materializeField(ctx, w, accept, h.Name, base,
				plainVal, refVal,
				func(ref string) { h.Config[refField] = ref })
			if err != nil {
				return err
			}
			delete(h.Config, base) // plaintext стёрт из config (даже junk-значение)
			wrote = wrote || did
		}
	}

	h.SecretWritten = wrote
	return nil
}

// materializeField обрабатывает одно секрет-поле: value(plaintext) XOR ref.
// Задан value → пишет в Vault (secret/herald/<entity>/<field>) и вызывает
// setRef(ref); возвращает did=true. Пусто/только ref — no-op (существующее
// поведение). Оба заданы → [ErrValidation]. plaintext + !accept (или w=nil) →
// [ErrPlaintextDisabled]. Значение секрета в ошибку не попадает.
func materializeField(ctx context.Context, w SecretWriter, accept bool, entity, field, value, ref string, setRef func(string)) (bool, error) {
	hasValue := value != ""
	hasRef := ref != ""
	if hasValue && hasRef {
		return false, wrapValidation(fmt.Errorf("%s and %s_ref are mutually exclusive (provide exactly one)", field, field))
	}
	if !hasValue {
		return false, nil
	}
	if !accept || w == nil {
		return false, wrapValidation(ErrPlaintextDisabled)
	}
	newRef, err := w.WriteString(ctx, secretwrite.DomainHerald, entity, field, value)
	if err != nil {
		// err от secretwrite не несёт значения секрета.
		return false, fmt.Errorf("herald: materialize %s secret: %w", field, err)
	}
	setRef(newRef)
	return true, nil
}

// ptrStr разыменовывает *string в "" при nil.
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
