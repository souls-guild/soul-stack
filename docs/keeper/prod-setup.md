# Прод-развёртывание Keeper-а

Операционная дока для перевода Keeper-а из dev-стека ([local-setup.md](../dev/local-setup.md)) в продакшен. Фокус — на отличиях от dev и на инфра-зависимостях (Vault, Postgres), которые не покрываются нашим кодом, но обязательны для безопасной эксплуатации.

Нормативный контракт конфига — [config.md](config.md); пример прод-конфига — [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml).

## Чем прод отличается от dev

| Аспект | dev ([local-setup.md](../dev/local-setup.md)) | прод |
|---|---|---|
| Vault auth | `vault.token: "root"` (статический root-токен) | AppRole (`vault.auth.method: approle`) — см. ниже |
| Vault backend | dev-mode, секреты в RAM (теряются на рестарте) | persistent storage backend + auto-unseal |
| Vault TLS | HTTP без TLS (`http://127.0.0.1:8200`) | HTTPS с валидным сертификатом (`https://vault.internal:8200`) |
| Vault-policy | root (всё разрешено) | узкая least-privilege policy ([vault-policy.hcl](../../examples/keeper/vault-policy.hcl)) |
| TLS-материал Keeper-а | self-signed / Vault-issued leaf в `/tmp/keeper-dev/tls/` | leaf из прод-PKI, ротация по политике организации |
| `services[]` / destiny | file://-репо из `/tmp/keeper-dev/` | git-URL-ы реестра сервисов (Postgres, ADR-029) |
| OTel | `otel.enabled: false` | включён, экспорт в коллектор |

dev-shortcut `vault.token: "root"` в проде **не использовать** — root-токен Vault не должен жить в конфиге сервиса. Прод-путь — AppRole + узкая policy.

## Vault: AppRole + persistent + auto-unseal

### AppRole-аутентификация Keeper-а

В проде Keeper аутентифицируется в Vault через AppRole (ADR-014; в коде — `shared/config.AuthMethodAppRole`). Keeper делает `auth/approle/login` с парой `role_id` + `secret_id`, получает renewable client-token и продлевает его (TokenRenewer, `keeper/internal/.../renewer.go`).

Блок `keeper.yml::vault`:

```yaml
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                       # НЕ секрет — идентификатор роли
    secret_id_file: /etc/keeper/vault-secret-id  # secret_id из файла mode 0400
  pki_mount: "pki/soulstack"
  pki_role: "soul-seed"
```

`role_id` — **не секрет** (идентификатор роли), допустимо хранить в открытом виде прямо в `keeper.yml`. `secret_id` — **секрет**, в основном конфиге plaintext-ом **не хранится**; источник задаётся одним из (взаимоисключающе):

- `secret_id_file` — путь к файлу с правами **`mode 0400`** (только владелец-процесс читает), содержимое = `secret_id` (trailing newline снимается). Рекомендуемый прод-вариант;
- `secret_id_env` — имя env-переменной с `secret_id` (для инжекторов секретов вроде Vault Agent / k8s-secret-as-env).

AppRole-credentials **намеренно НЕ читаются из Vault** (`vault:`-ref недопустим) — это chicken-egg: именно этими credentials Keeper и логинится, чтобы потом резолвить все остальные `vault:`-ref-ы (`postgres.dsn_ref`, `signing_key_ref`, …). Источник `secret_id` поэтому всегда локальный (файл/env), до подъёма Vault-клиента. Детали контракта — [config.md → `vault`](config.md#vault), comment в `shared/config/keeper.go` (`KeeperVaultAuth`).

Настройка роли на стороне Vault (привязка least-privilege policy из п. ниже):

```sh
vault policy write keeper-prod examples/keeper/vault-policy.hcl
vault write auth/approle/role/keeper-prod \
    token_policies=keeper-prod \
    secret_id_ttl=720h token_ttl=1h token_max_ttl=24h
# role_id — выдать оператору (в keeper.yml):
vault read auth/approle/role/keeper-prod/role-id
# secret_id — положить в /etc/keeper/vault-secret-id (mode 0400):
vault write -f auth/approle/role/keeper-prod/secret-id
```

### Least-privilege Vault-policy

Keeper в проде работает под узкой policy, а не под root. Шаблон с покомментированными path-ами — [`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl). Кратко, что он выдаёт и почему минимально:

| Path | Capabilities | Зачем |
|---|---|---|
| `secret/data/keeper/*` | `read` | Чтение KV-секретов Keeper-а (jwt-signing-key, postgres/redis, providers/credentials cloud-driver-ов, essence-секреты). Только read — Keeper их не пишет. |
| `pki/issue/soul-seed` | `update` | Подпись CSR SoulSeed при онбординге (Bootstrap-RPC, ADR-012(b)). `update` (POST) — единственное, что нужно issue-эндпоинту. |
| `secret/metadata/keeper/sigil-keys/*` | `list`, `read` | Reaper-правило `reap_orphan_vault_keys` (ADR-026(h)) — report-only reconcile осиротевших ключей подписи Sigil: только имена (`list`) + `created_time` (metadata). **БЕЗ `delete`, БЕЗ data-пути** — Reaper ничего не удаляет и не читает значения приватников. |
| `auth/token/renew-self` | `update` | TokenRenewer продлевает собственный client-token Keeper-а. Без права создавать/ревокать чужие токены. |

Пути в шаблоне соответствуют дефолтам dev-провижининга (KV mount `secret/`, PKI mount `pki/` + role `soul-seed` — см. [`dev/provision.sh`](../../dev/provision.sh)). В проде KV/PKI mount могут отличаться (в примере конфига `pki_mount: "pki/soulstack"`) — поправьте префиксы путей в `.hcl` под фактические `keeper.yml::vault.kv_mount` / `pki_mount` / `pki_role`.

### Persistent backend + auto-unseal (инфра-зависимость)

Это **инфраструктурная зависимость Vault**, не часть кода Soul Stack — операционные заметки:

- **Persistent storage backend** обязателен. dev-mode Vault держит секреты в RAM и теряет их при рестарте — в проде это означало бы потерю jwt-signing-key (инвалидация всех JWT) и PKI-корня. Использовать persistent backend (raft / consul / поддерживаемый storage).
- **Auto-unseal** настоятельно рекомендуется (cloud KMS / transit / HSM). Без него каждый рестарт Vault требует ручного unseal с кворумом ключей, что ломает автоматический recovery Keeper-кластера: пока Vault sealed, Keeper не резолвит `vault:`-ref-ы и не стартует.
- Подъём/тюнинг этих компонентов — на стороне команды эксплуатации Vault; Soul Stack только потребляет готовый unsealed Vault по `keeper.yml::vault.addr`.

## JWT signing-key (прод)

JWT операторов (ADR-014) подписываются ключом из KV `secret/keeper/jwt-signing-key` (поле `signing_key`), резолв через `auth.jwt.signing_key_ref`. На MVP это HS256-ключ (симметричный) в KV — приемлемо; transit-вариант (асимметричная подпись на стороне Vault) — post-MVP.

**Ротация signing-key** — операционная процедура (не автоматизирована):

1. Сгенерировать новый ключ и записать в KV: `vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"`.
2. Передеплоить / hot-reload Keeper, чтобы он перечитал ключ.
3. **Пересоздать bootstrap-токены / перевыпустить операторские JWT** — все ранее выпущенные HS256-JWT после смены ключа становятся невалидны (подпись не сойдётся). Короткий TTL JWT (`auth.jwt.ttl_default`) ограничивает окно, но активные токены придётся перевыпустить явно.

Поэтому ротацию планировать в окно обслуживания, а не «на ходу».

## См. также

- [local-setup.md](../dev/local-setup.md) — dev-стек (для прода смотреть эту страницу, dev-копию конфига не использовать).
- [config.md](config.md) — нормативный контракт `keeper.yml` (блоки `vault`, `auth`, `reaper`, `acolytes`).
- [vault-policy.hcl](../../examples/keeper/vault-policy.hcl) — least-privilege Vault-policy с покомментированными путями.
- [reaper.md → Включение recovery](reaper.md#включение-recovery-recovery-enable) — отдельная гейт-процедура для `reclaim_apply_runs` в проде.
- [rbac.md](rbac.md) — RBAC и Bootstrap первого Архонта (ADR-013).
