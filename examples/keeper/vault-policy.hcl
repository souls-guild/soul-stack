# keeper-prod — least-privilege Vault-policy для прод-инстанса Keeper-а.
#
# Привязывается к AppRole, которым Keeper логинится в Vault
# (keeper.yml::vault.auth.method=approle, role_id=keeper-prod —
# см. docs/keeper/prod-setup.md и shared/config.AuthMethodAppRole).
#
# Принцип: каждый path выдаёт РОВНО те capabilities, что нужны конкретной
# подсистеме Keeper-а, и ни на одну больше. Никаких `*`-capabilities,
# никаких широких mount-уровневых грантов. Менять под свою инсталляцию
# нужно только конкретные пути (mount KV / PKI), не набор capabilities.
#
# Применение (после `vault policy write keeper-prod vault-policy.hcl`):
#   vault write auth/approle/role/keeper-prod \
#       token_policies=keeper-prod \
#       secret_id_ttl=... token_ttl=... token_max_ttl=...
#
# ВАЖНО: пути ниже соответствуют дефолтам dev-провижининга
# (dev/provision.sh): KV mount `secret/`, PKI mount `pki/` + role `soul-seed`.
# В проде KV-mount и PKI-mount могут отличаться (в examples/keeper/keeper.yml
# показан pki_mount: "pki/soulstack") — поправьте префиксы путей под
# фактические keeper.yml::vault.kv_mount / pki_mount / pki_role.

# --- KV v2: чтение секретов Keeper-а ----------------------------------------
#
# Все runtime-секреты Keeper-а живут под secret/keeper/* (KV v2). Read-only:
# Keeper их только читает (резолв `vault:`-ref на старте и при hot-reload),
# никогда не пишет. KV v2 хранит ЗНАЧЕНИЯ под data-путём `secret/data/...`.
# Сюда входят:
#   - secret/keeper/jwt-signing-key  (auth.jwt.signing_key_ref, HS256-ключ, ADR-014)
#   - secret/keeper/postgres         (postgres.dsn_ref, поле `dsn`)
#   - secret/keeper/redis            (redis.password_ref)
#   - secret/keeper/providers/*      (credentials cloud-driver-ов, ADR-017)
#   - essence-секреты под secret/keeper/* (резолв `${ vault(...) }` в CEL)
#
# `read` достаточно: для чтения значения KV v2 list/data-write не нужны.
path "secret/data/keeper/*" {
  capabilities = ["read"]
}

# --- KV v2: dual-mode приём секрета оператора (ADR-064, NIM-11) ---------------
#
# При plaintext-приёме секрета (Herald signing/channel-token, Provider cloud-
# credentials) Keeper САМ пишет значение в Vault по детерминированному пути
# secret/<domain>/<entity>/<field> и хранит в Postgres только ref. KV v2 держит
# ЗНАЧЕНИЯ под data-путём. Нужны create+update (idempotent-write при update
# перезаписывает по тому же пути) + read (резолв на потреблении: herald-доставка
# читает signing/channel-token, cloud-flow читает credentials).
#
# Скоуп узкий (herald/*, provider/*), НЕ весь mount: keeper пишет ТОЛЬКО в свои
# детерминированные префиксы. При кастомном kv_mount (keeper.yml::vault.kv_mount)
# замените `secret/` на фактический mount.
path "secret/data/herald/*" {
  capabilities = ["create", "update", "read"]
}
path "secret/data/provider/*" {
  capabilities = ["create", "update", "read"]
}

# --- KV v2: reveal-раскрытие секретов инкарнации (NIM-74) --------------------
#
# Оператор с правом incarnation.view-secrets раскрывает plaintext секрета,
# объявленного `revealable_secrets` сервиса (POST .../secrets/reveal). Keeper
# резолвит значение keeper-side по vault_ref сервиса. Для redis-сервиса
# vault_ref = secret/redis/<incarnation>/users/<key>#password → нужен read на
# префикс, объявляемый манифестом. Read-only (значение только читается).
# Скоуп узкий (redis/*), не весь mount; при кастомном kv_mount/пути в
# revealable_secrets — поправьте префикс под свой манифест.
path "secret/data/redis/*" {
  capabilities = ["read"]
}

# --- PKI: подпись CSR SoulSeed при онбординге -------------------------------
#
# При Bootstrap-RPC (ADR-012(b)) Keeper выписывает SoulSeed-сертификат
# через PKI issue-role `soul-seed` (dev/provision.sh: `pki/issue/soul-seed`).
# `update` (== POST) — единственная нужная capability для issue/sign-эндпоинта;
# Keeper не управляет ролями/корнем PKI, только выписывает leaf по готовой роли.
# Замените `pki/` + `soul-seed` на ваши keeper.yml::vault.pki_mount / pki_role.
path "pki/issue/soul-seed" {
  capabilities = ["update"]
}

# --- Reaper: report-only reconcile осиротевших ключей подписи Sigil ----------
#
# Правило reap_orphan_vault_keys (ADR-026(h), reaper.md → Правила) находит
# приватники подписи Sigil в Vault, для которых нет строки в реестре
# sigil_signing_keys. Оно ТОЛЬКО считает/метрит/логирует находку:
#   - `list` — перечислить key_id под набором ключей подписи Sigil;
#   - `read` — прочитать `created_time` из METADATA-слоя (для grace по возрасту).
#
# Намеренно НЕТ:
#   - `delete` — Reaper НИЧЕГО не удаляет из Vault (report-only);
#   - доступа к data-пути `secret/data/keeper/sigil-keys/*` — Reaper НЕ читает
#     ЗНАЧЕНИЯ приватников, только metadata (имена + created_time).
# Это держит blast radius Жнеца минимальным: он не может ни прочитать
# приватный ключ подписи, ни удалить его.
path "secret/metadata/keeper/sigil-keys/*" {
  capabilities = ["list", "read"]
}

# --- Self-renew client-token ------------------------------------------------
#
# Keeper логинится через AppRole и получает renewable client-token; TokenRenewer
# (keeper renewer.go) продлевает его до token_max_ttl, чтобы Keeper не терял
# доступ к Vault на длинном uptime. Нужна ровно одна capability на renew-self
# собственного токена — без права создавать/ревокать чужие токены.
path "auth/token/renew-self" {
  capabilities = ["update"]
}
