package push

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// hostCAField — имя поля в Vault KV, из которого читается PEM-encoded SSH
// public key host-CA (`push.host_ca_ref` / `push.host_ca_refs[].ref`).
// Симметрия с `signing_key` (auth.jwt) / `password` (metrics.auth.basic):
// в Vault KV хранится ровно одно поле, его имя зафиксировано конвенцией.
const hostCAField = "public_key"

// NamedHostKeyAuthority — один CA в multi-CA-наборе (S7-3, ADR-032 amendment
// 2026-05-26). Имя — operator-defined (или auto-name `default` при backward-
// compat adapt singular `host_ca_ref`); используется как label-значение в
// `keeper_push_host_ca_used_total{ca_name=...}` и в diag-сообщениях.
//
// SourceRef сохраняется для логов / fail-fast-ошибок (без него на ошибку
// «парс PEM упал» не понять, какой ref сломан).
type NamedHostKeyAuthority struct {
	Name      string
	CAPubKey  ssh.PublicKey
	SourceRef string
}

// KVReader — узкое чтение KV из Vault, нужное [LoadHostCA]. Удовлетворяется
// keeper-side *keepervault.Client; вынесено отдельно ради unit-тестируемости
// без подъёма Vault-клиента.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// LoadHostCA читает public host-CA из Vault по vault-ref-у (`vault:<mount>/<path>`)
// и возвращает [HostKeyAuthority] для инжекции в [SshDispatcher.Deps.HostAuthority].
//
// Vault KV-объект должен иметь поле `public_key` со значением одного из:
//   - OpenSSH authorized_keys-форма (`ssh-ed25519 AAAA... [comment]`);
//   - PEM-блок SSH PUBLIC KEY (поддерживается через `ssh.ParseAuthorizedKey` для
//     authorized-форм; PEM-обёртка не предусмотрена pilot-схемой, оператор
//     кладёт authorized-key как наиболее частую форму вывода `ssh-keygen -y`).
//
// Любая ошибка резолва (нет ref-а / Vault недоступен / отсутствует поле /
// невалидная форма) — fail-fast: caller (daemon setupPushDispatchers) валит
// старт keeper-а, иначе push-диспетчер уйдёт без host-CA и любой connect
// провалится с невнятной ошибкой.
func LoadHostCA(ctx context.Context, vc KVReader, ref string) (HostKeyAuthority, error) {
	if vc == nil {
		return HostKeyAuthority{}, errors.New("push: LoadHostCA: vault client is nil")
	}
	if ref == "" {
		return HostKeyAuthority{}, errors.New("push: LoadHostCA: empty vault ref")
	}
	path, err := keepervault.ParseRef(ref)
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: read vault %q: %w", path, err)
	}
	raw, ok := kv[hostCAField]
	if !ok {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: vault secret %q has no %q field", path, hostCAField)
	}
	pemStr, ok := raw.(string)
	if !ok || pemStr == "" {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: vault field %q is empty or not a string", hostCAField)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pemStr))
	if err != nil {
		return HostKeyAuthority{}, fmt.Errorf("push: LoadHostCA: parse host-CA public key: %w", err)
	}
	return HostKeyAuthority{CAPublicKey: pub}, nil
}

// LoadHostCAs резолвит множество vault-ref-ов в [NamedHostKeyAuthority]-набор
// для multi-CA verify host-keys (S7-3, ADR-032 amendment 2026-05-26).
// Переиспользует [LoadHostCA] на каждый элемент — единственная точка чтения
// Vault KV / парсинга PEM. При ошибке резолва одного из ref-ов — fail-fast с
// именем CA в обёртке (caller `setupPushDispatchers` валит старт keeper-а).
//
// Пустой `refs` → nil, nil: caller сам решает, fail это или valid case
// (singular path / push выключен). Daemon валит старт раньше — на gate-проверке
// «host_ca_ref/refs не задан».
func LoadHostCAs(ctx context.Context, vc KVReader, refs []config.KeeperPushCARef) ([]NamedHostKeyAuthority, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if vc == nil {
		return nil, errors.New("push: LoadHostCAs: vault client is nil")
	}
	out := make([]NamedHostKeyAuthority, 0, len(refs))
	for _, r := range refs {
		ca, err := LoadHostCA(ctx, vc, r.Ref)
		if err != nil {
			return nil, fmt.Errorf("push: LoadHostCAs[%s]: %w", r.Name, err)
		}
		out = append(out, NamedHostKeyAuthority{
			Name:      r.Name,
			CAPubKey:  ca.CAPublicKey,
			SourceRef: r.Ref,
		})
	}
	return out, nil
}
