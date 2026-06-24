// Package vault — обёртка над `github.com/hashicorp/vault/api` для чтения
// Vault KV (v1/v2) секретов на keeper-стороне.
//
// Scope (ADR-014, ADR-017): чтение `secret/keeper/jwt-signing-key` для
// JWT-issuer-а в bootstrap-логике, поддержка `core.vault.kv-read` на
// keeper-side.
//
// Аутентификация (cfg.Auth.Method):
//   - `token` (default, dev) — статический токен из cfg.Token;
//   - `approle` (прод, ADR-014) — `auth/approle/login` с role_id + secret_id;
//     возвращает renewable client-token.
//
// Auto-renew токена — см. renewer.go (TokenRenewer на vault LifetimeWatcher);
// renewable-токен (approle) продлевается в фоне, non-renewable (root/dev)
// деградирует без падения.
//
// Post-MVP:
//   - TLS CA (cfg.Addr=https://..., custom CA bundle).
//   - Namespace (Vault Enterprise).
package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/souls-guild/soul-stack/shared/config"
)

// defaultKVMount — fallback для `cfg.KVMount`, когда пустой в keeper.yml.
// Это дефолтный mount Vault dev-mode (`vault secrets enable -path=secret kv-v2`
// активируется автоматически).
const defaultKVMount = "secret"

// ErrVaultKVNotFound — путь KV не существует или удалён (soft delete).
// Отдельный sentinel позволяет вызывающему коду различать «нет ключа»
// от транспортных ошибок.
var ErrVaultKVNotFound = errors.New("vault: KV path not found")

// kvVersionUnset — `kvVersionOverride` не задан (auto-detect через probe).
const kvVersionUnset = 0

// Client — тонкая обёртка над *vaultapi.Client с фиксированным KV-mount.
//
// Безопасен для конкурентного использования: vaultapi.Client держит
// внутренний http.Client с пулом соединений. Кеш версий KV mount-ов
// (`kvVersions`) защищён `kvVersionsMu` — `resolveKVVersion` конкурентен.
type Client struct {
	c       *vaultapi.Client
	kvMount string
	metrics *VaultMetrics

	// kvVersionOverride — форс версии KV из cfg.Vault.KVVersion (1/2);
	// kvVersionUnset → auto-detect через probe (sys/internal/ui/mounts).
	kvVersionOverride int

	// kvVersions — per-mount кеш резолвленной версии KV (1/2). Probe ленивый
	// (первый ReadKV/WriteKV mount-а), результат кешируется. map, а не одно
	// поле, чтобы расширяться на мультимаунтовые сценарии без переписывания.
	kvVersionsMu sync.RWMutex
	kvVersions   map[string]int
}

// SetMetrics подключает keeper_vault_*-метрики к клиенту (ADR-024). nil-safe
// no-op-методы [VaultMetrics] позволяют не ставить метрики вовсе (bootstrap-путь
// keeper init поднимает Client до создания obs.Registry). Вызывается один раз в
// keeper run после [RegisterVaultMetrics]; конкурентных писателей нет — клиент к
// этому моменту ещё не отдан в render/scenario/CEL.
func (c *Client) SetMetrics(m *VaultMetrics) { c.metrics = m }

// approleLoginPath — endpoint AppRole-аутентификации Vault.
const approleLoginPath = "auth/approle/login"

// NewClient создаёт Vault-client по cfg, аутентифицируется выбранным методом
// и проверяет связность через Ping.
//
// cfg.Addr — обязателен (например, "http://localhost:8200" для dev,
// "https://vault.example.com:8200" для prod).
// cfg.KVMount — путь до KV v2 secrets engine, default "secret".
//
// Аутентификация по cfg.Auth.ResolvedAuthMethod():
//   - token: cfg.Token устанавливается напрямую (для dev = "root").
//   - approle: `auth/approle/login` с role_id + secret_id (из file/env);
//     полученный renewable client-token устанавливается на client; продление
//     дальше — TokenRenewer (renewer.go).
//
// Post-MVP: TLS CA / namespace переезжают в cfg.
func NewClient(ctx context.Context, cfg config.KeeperVault) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("vault: addr is empty")
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = cfg.Addr
	if err := apiCfg.Error; err != nil {
		return nil, fmt.Errorf("vault: api default config: %w", err)
	}

	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault: NewClient: %w", err)
	}

	switch cfg.Auth.ResolvedAuthMethod() {
	case config.AuthMethodToken:
		if cfg.Token == "" {
			return nil, errors.New("vault: token is empty (auth.method=token)")
		}
		api.SetToken(cfg.Token)

	case config.AuthMethodAppRole:
		if err := loginAppRole(ctx, api, cfg.Auth); err != nil {
			return nil, err
		}

	default:
		// Schema-фаза config-а ограничивает method enum-ом; сюда попасть
		// можно только при рассинхроне config-схемы и этого switch-а.
		return nil, fmt.Errorf("vault: unsupported auth method %q", cfg.Auth.Method)
	}

	// trailing slash в mount → silent wrong-path при формировании URL
	// (`secret//data/...`). Нормализуем перед сохранением.
	mount := strings.TrimSuffix(cfg.KVMount, "/")
	if mount == "" {
		mount = defaultKVMount
	}

	// kv_version override (escape-hatch): пусто → kvVersionUnset (probe).
	// Невалидное значение отсекается schema-фазой; здесь fail-fast для
	// callers вне config-load-пути (тесты).
	override := kvVersionUnset
	switch cfg.KVVersion {
	case "":
		// auto-detect
	case "1":
		override = 1
	case "2":
		override = 2
	default:
		return nil, fmt.Errorf("vault: invalid kv_version %q (want \"1\" or \"2\")", cfg.KVVersion)
	}

	cl := &Client{
		c:                 api,
		kvMount:           mount,
		kvVersionOverride: override,
		kvVersions:        make(map[string]int),
	}
	// Probe версии KV — строго ленивый (первый ReadKV/WriteKV). Здесь только
	// Ping (sys/health, без KV-прав) — bootstrap-путь (keeper init) поднимает
	// Client до выдачи KV-доступа; probe в конструкторе сломал бы его.
	if err := cl.Ping(ctx); err != nil {
		return nil, fmt.Errorf("vault: ping: %w", err)
	}
	return cl, nil
}

// resolveKVVersion определяет версию KV secrets engine для mount-а (1 или 2).
//
// Порядок:
//  1. override (cfg.Vault.kv_version) — побеждает безусловно, БЕЗ round-trip-а;
//  2. per-mount кеш — версия уже резолвлена;
//  3. probe `sys/internal/ui/mounts/<mount>` → `data.type == "kv"` +
//     `data.options.version` ("1"/"2").
//
// Fail-closed: если probe не дал однозначной версии («kv» mount без
// options.version / неожиданное значение / type != "kv" / permission-denied
// без override) — явная ошибка с подсказкой про `vault.kv_version`, а НЕ
// молчаливый дефолт v2. Прежний механизм «угадывания по классу ошибки
// KVv2.Get» отвергнут (обычный v1-секрет неотличим от v2-missing).
//
// Probe ленивый — вызывается из ReadKV/WriteKV, не из NewClient (см. там).
func (c *Client) resolveKVVersion(ctx context.Context, mount string) (int, error) {
	if c.kvVersionOverride != kvVersionUnset {
		return c.kvVersionOverride, nil
	}

	c.kvVersionsMu.RLock()
	v, ok := c.kvVersions[mount]
	c.kvVersionsMu.RUnlock()
	if ok {
		return v, nil
	}

	// Re-check под write-lock перед probe: на холодном старте несколько горутин
	// видят промах RLock-кеша одновременно; без этого они сделают дублирующие
	// probe-round-trip-ы. Double-checked locking убирает лишние probe (single-
	// flight не нужен — probe идемпотентен, важна лишь экономия round-trip-ов).
	c.kvVersionsMu.Lock()
	defer c.kvVersionsMu.Unlock()
	if v, ok := c.kvVersions[mount]; ok {
		return v, nil
	}

	version, err := c.probeKVVersion(ctx, mount)
	if err != nil {
		return 0, err
	}
	c.kvVersions[mount] = version
	return version, nil
}

// probeKVVersion читает `sys/internal/ui/mounts/<mount>` и извлекает версию
// KV engine из `data.options.version`. Не кеширует (это делает
// resolveKVVersion). Ошибки fail-closed — см. resolveKVVersion.
func (c *Client) probeKVVersion(ctx context.Context, mount string) (int, error) {
	const hint = "specify vault.kv_version: 1|2 to skip auto-detect"

	secret, err := c.c.Logical().ReadWithContext(ctx, "sys/internal/ui/mounts/"+mount)
	if err != nil {
		// 403/permission-denied (ACL закрыл probe-endpoint) и любой другой
		// транспортный сбой — fail-closed: версию определить не вышло.
		// Ошибка транспорта/ACL probe-endpoint (sys/internal/ui/mounts) безопасна
		// для текста: секрета KV тут нет — включаем `%v` для диагностики (ACL/сеть).
		return 0, fmt.Errorf("vault: cannot determine KV version of mount %q via sys/internal/ui/mounts (%v); %s", mount, err, hint)
	}
	if secret == nil || secret.Data == nil {
		return 0, fmt.Errorf("vault: cannot determine KV version of mount %q: empty sys/internal/ui/mounts response; %s", mount, hint)
	}

	if t, _ := secret.Data["type"].(string); t != "kv" {
		return 0, fmt.Errorf("vault: mount %q is not a KV engine (type=%q); %s", mount, t, hint)
	}

	opts, ok := secret.Data["options"].(map[string]any)
	if !ok || opts == nil {
		return 0, fmt.Errorf("vault: mount %q has no KV options.version; %s", mount, hint)
	}
	ver, _ := opts["version"].(string)
	switch ver {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, fmt.Errorf("vault: mount %q has unexpected KV version %q; %s", mount, ver, hint)
	}
}

// requireKVv2 — guard для metadata/list-операций (ListKV / ReadKVMetadata):
// у KV v1 нет `<mount>/metadata/`-пути, поэтому на v1-mount-е эти операции
// бессмысленны. Отдаём понятную ошибку вместо молча-сломанного round-trip-а.
// op — человекочитаемое имя операции для текста ошибки ("list"/"metadata read").
func (c *Client) requireKVv2(ctx context.Context, op string) error {
	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return err
	}
	if version != 2 {
		return fmt.Errorf("vault: %s requires KV v2, but mount %q is v%d", op, c.kvMount, version)
	}
	return nil
}

// loginAppRole выполняет `auth/approle/login` и устанавливает полученный
// client-token на api. role_id берётся из конфига (не секрет), secret_id —
// из file/env (читается loadSecretID).
//
// Безопасность: ни role_id, ни secret_id, ни полученный токен в текст ошибок
// НЕ попадают — login-ошибки оборачиваются обобщённо. Сам vaultapi на
// HTTP-ошибку login-а тело ответа (без креденшелов) может приложить — это
// диагностика самого Vault-сервера, не наш секрет.
func loginAppRole(ctx context.Context, api *vaultapi.Client, auth config.KeeperVaultAuth) error {
	if auth.RoleID == "" {
		// Дублирует schema-валидацию, но NewClient вызывается и вне
		// config-load-пути (тесты, future callers) — fail-fast здесь.
		return errors.New("vault: approle login requires role_id")
	}
	secretID, err := loadSecretID(auth)
	if err != nil {
		return err
	}

	secret, err := api.Logical().WriteWithContext(ctx, approleLoginPath, map[string]any{
		"role_id":   auth.RoleID,
		"secret_id": secretID,
	})
	if err != nil {
		return fmt.Errorf("vault: approle login failed: %w", err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return errors.New("vault: approle login returned no client token")
	}
	api.SetToken(secret.Auth.ClientToken)
	return nil
}

// loadSecretID читает secret_id из сконфигурированного источника:
// secret_id_file (mode-ограниченный файл) либо secret_id_env (env-переменная).
// Trailing whitespace/newline снимается. Источник ровно один — гарантия
// schema-фазы; здесь повторно проверяем для callers вне config-load-пути.
//
// Безопасность: значение secret_id в сообщения об ошибках НЕ попадает —
// только путь к файлу / имя env-переменной (это не секреты).
func loadSecretID(auth config.KeeperVaultAuth) (string, error) {
	switch {
	case auth.SecretIDFile != "":
		raw, err := os.ReadFile(auth.SecretIDFile)
		if err != nil {
			return "", fmt.Errorf("vault: read secret_id_file %q: %w", auth.SecretIDFile, err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("vault: secret_id_file %q is empty", auth.SecretIDFile)
		}
		return v, nil

	case auth.SecretIDEnv != "":
		v := strings.TrimSpace(os.Getenv(auth.SecretIDEnv))
		if v == "" {
			return "", fmt.Errorf("vault: secret_id_env %q is empty or unset", auth.SecretIDEnv)
		}
		return v, nil

	default:
		return "", errors.New("vault: approle login requires secret_id_file or secret_id_env")
	}
}

// relativeKVPath нормализует входной KV-путь к relative-форме (без mount-
// префикса) и проверяет его на безопасность. Общий преамбул всех KV-методов
// (ReadKV / WriteKV / ListKV / ReadKVMetadata).
//
// Шаги:
//   - снимаем leading slash (иначе `/secret/foo` не сведётся к `foo`);
//   - отсекаем mount-prefix, если path задан в logical-форме;
//   - повторно снимаем leading slash;
//   - отвергаем пустой результат (нет смысла в round-trip без пути).
//
// БЕЗОПАСНОСТЬ (defense-in-depth): fail-closed на `..`-сегмент. Литеральный
// `../` коллапсится Go/HTTP-клиентом при формировании URL и может вывести
// запрос за пределы заданного mount/scope. Текущие caller-ы такие пути не
// строят (orphan-скан читает только имена из ListKV), но guard защищает
// будущих. Проверяем по результату path.Clean: если Clean схлопнул `..`
// вверх по дереву (`..` остался в начале) — отказ; и явным сканом сегментов
// на случай `a/../b` внутри.
func (c *Client) relativeKVPath(input string) (string, error) {
	input = strings.TrimPrefix(input, "/")
	rel := strings.TrimPrefix(input, c.kvMount+"/")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return "", fmt.Errorf("vault: empty KV path after stripping mount %q", c.kvMount)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("vault: path contains '..' segment: %q", input)
		}
	}
	// Дополнительная страховка: path.Clean схлопывает escaping-последовательности;
	// ведущий `..` после Clean означает выход за scope.
	if cleaned := path.Clean(rel); strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("vault: path contains '..' segment: %q", input)
	}
	return rel, nil
}

// ReadKV читает значение секрета по path. Контракт возврата — единый плоский
// payload (`map = поля секрета`), независимо от версии KV mount-а: KVv2.Get
// уже отдаёт развёрнутый `data.data`, KVv1.Get — `secret.Data` плоско.
//
// `path` принимается в двух формах:
//   - logical: "secret/keeper/jwt-signing-key" (с префиксом mount-а);
//   - relative: "keeper/jwt-signing-key" (mount подставляется автоматически).
//
// Mount резолвится через client.kvMount; версия (v1/v2) — через
// resolveKVVersion (override или probe). Возвращает ErrVaultKVNotFound,
// если путь отсутствует или удалён (Vault отдаёт пустой Secret) — для ОБЕИХ
// версий.
func (c *Client) ReadKV(ctx context.Context, path string) (_ map[string]any, err error) {
	// Нормализация input-path (strip mount/leading-slash) + fail-closed
	// guard на `..`-сегмент — см. relativeKVPath.
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return nil, err
	}

	// keeper_vault_*-метрики (ADR-024): латентность round-trip-а + counter
	// ошибок (notfound/error). Замер с этой точки покрывает сетевой вызов и
	// разбор результата; label — только mount (не путь-с-секретом). nil
	// metrics → no-op. Пустой-путь-guard выше — структурный отказ caller-а,
	// не Vault-round-trip, его не измеряем.
	start := time.Now()
	defer func() { c.metrics.ObserveRead(c.kvMount, time.Since(start), err) }()

	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return nil, err
	}

	var secret *vaultapi.KVSecret
	if version == 2 {
		secret, err = c.c.KVv2(c.kvMount).Get(ctx, rel)
	} else {
		secret, err = c.c.KVv1(c.kvMount).Get(ctx, rel)
	}
	if err != nil {
		// vaultapi.KVv{1,2}.Get возвращают ErrSecretNotFound для отсутствующих
		// и tombstone-нутых путей; маппим в наш sentinel (обе версии).
		if errors.Is(err, vaultapi.ErrSecretNotFound) {
			err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
			return nil, err
		}
		err = fmt.Errorf("vault: read %q: %w", path, err)
		return nil, err
	}
	if secret == nil || secret.Data == nil {
		err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
		return nil, err
	}
	return secret.Data, nil
}

// WriteKV записывает секрет в KV по path (KV v2 создаёт новую версию). data —
// плоский набор полей секрета (`{"signing_key": "<PEM>"}`).
//
// `path` принимается в тех же формах, что [Client.ReadKV] (logical с mount-
// префиксом либо relative); mount/версия резолвятся через client.kvMount +
// resolveKVVersion. Симметрично чтению: leading slash снимается, mount-prefix
// отсекается.
//
// Scope (ADR-026(h), R3-S7): запись приватника ed25519-ключа подписи Sigil при
// вводе нового trust-anchor-ключа (`secret/keeper/sigil-keys/<key_id>`). До R3
// keeper только читал Vault (jwt-/sigil-signing-key, core.vault.kv-read); запись
// вводится здесь как минимальная зеркальная поверхность чтения.
//
// БЕЗОПАСНОСТЬ: значения полей секрета (в т.ч. приватник) в текст ошибок НЕ
// попадают — только path (имя секрета, не его содержимое).
func (c *Client) WriteKV(ctx context.Context, path string, data map[string]any) (err error) {
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("vault: empty data for KV write %q", path)
	}

	// keeper_vault_*-метрики: латентность write round-trip-а + counter ошибок.
	// label — только mount (не путь-с-секретом), как у ObserveRead. nil metrics
	// → no-op. Пустой-путь/data-guard выше — структурный отказ caller-а, не
	// Vault-round-trip, его не измеряем.
	start := time.Now()
	defer func() { c.metrics.ObserveWrite(c.kvMount, time.Since(start), err) }()

	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return err
	}

	if version == 2 {
		_, err = c.c.KVv2(c.kvMount).Put(ctx, rel, data)
	} else {
		err = c.c.KVv1(c.kvMount).Put(ctx, rel, data)
	}
	if err != nil {
		err = fmt.Errorf("vault: write %q: %w", path, err)
		return err
	}
	return nil
}

// ListKV перечисляет имена секретов под prefix на metadata-пути KV v2
// (`<mount>/metadata/<rel>`). Возвращает последний сегмент каждого имени
// (key_id для `keeper/sigil-keys/<key_id>`), НЕ полный путь.
//
// `prefix` принимается в тех же двух формах, что [Client.ReadKV] (logical с
// mount-префиксом либо relative); mount резолвится через client.kvMount.
//
// Пустой/отсутствующий prefix (Vault отдаёт nil-Secret для несуществующей
// подпапки) → (nil, nil): это валидное «секретов под prefix нет», НЕ ошибка.
// Так orphan-reconcile отличает «сирот нет» от транспортного сбоя.
//
// Scope (ADR-026(h), reap_orphan_vault_keys): перечисление
// `secret/keeper/sigil-keys/` для поиска осиротевших приватников подписи.
// Требует Vault-policy с `list` на `secret/metadata/keeper/sigil-keys/*`.
// Значения секретов НЕ читаются — только имена с metadata-пути.
func (c *Client) ListKV(ctx context.Context, prefix string) (_ []string, err error) {
	rel, err := c.relativeKVPath(prefix)
	if err != nil {
		return nil, err
	}

	// keeper_vault_list_*-метрики: латентность LIST round-trip-а + counter
	// ошибок. label — только mount (не путь-с-именами секретов), как у
	// ObserveRead. nil-secret (пустая/отсутствующая подпапка) — НЕ ошибка
	// (kind не инкрементится). Пустой-prefix-guard выше — структурный отказ
	// caller-а, не Vault-round-trip, его не измеряем.
	start := time.Now()
	defer func() { c.metrics.ObserveList(c.kvMount, time.Since(start), err) }()

	if err = c.requireKVv2(ctx, "list"); err != nil {
		return nil, err
	}

	secret, err := c.c.Logical().ListWithContext(ctx, c.kvMount+"/metadata/"+rel)
	if err != nil {
		err = fmt.Errorf("vault: list %q: %w", prefix, err)
		return nil, err
	}
	// Несуществующая подпапка → nil-Secret или пустой Data. Сирот нет.
	if secret == nil || secret.Data == nil {
		return nil, nil
	}
	rawKeys, ok := secret.Data["keys"].([]any)
	if !ok {
		// Отсутствие ключа `keys` или неожиданный тип — пустой LIST-ответ.
		return nil, nil
	}

	names := make([]string, 0, len(rawKeys))
	for _, rk := range rawKeys {
		name, ok := rk.(string)
		if !ok || name == "" {
			continue
		}
		// Trailing-slash → подпапка. У плоского `sigil-keys/` их быть не
		// должно; фильтруем defensive, чтобы не принять подпапку за key_id.
		if strings.HasSuffix(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// ReadKVMetadata читает version-agnostic metadata секрета (`created_time`) с
// metadata-пути KV v2 (`<mount>/metadata/<rel>`). Нужно для grace по возрасту
// в orphan-reconcile — без чтения самого секрета.
//
// `path` принимается в тех же формах, что [Client.ReadKV]; mount резолвится
// через client.kvMount. Возвращает [ErrVaultKVNotFound], если metadata нет.
//
// БЕЗОПАСНОСТЬ: читается ТОЛЬКО metadata-путь — значение секрета (приватник)
// не запрашивается. Метрики — переиспользуют [VaultMetrics.ObserveRead]
// (metadata-read = частный случай чтения).
func (c *Client) ReadKVMetadata(ctx context.Context, path string) (_ time.Time, err error) {
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return time.Time{}, err
	}

	start := time.Now()
	defer func() { c.metrics.ObserveRead(c.kvMount, time.Since(start), err) }()

	if err = c.requireKVv2(ctx, "metadata read"); err != nil {
		return time.Time{}, err
	}

	secret, err := c.c.Logical().ReadWithContext(ctx, c.kvMount+"/metadata/"+rel)
	if err != nil {
		err = fmt.Errorf("vault: read metadata %q: %w", path, err)
		return time.Time{}, err
	}
	if secret == nil || secret.Data == nil {
		err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
		return time.Time{}, err
	}
	rawCreated, ok := secret.Data["created_time"].(string)
	if !ok || rawCreated == "" {
		err = fmt.Errorf("vault: metadata %q has no created_time", path)
		return time.Time{}, err
	}
	created, perr := time.Parse(time.RFC3339Nano, rawCreated)
	if perr != nil {
		err = fmt.Errorf("vault: parse created_time for %q: %w", path, perr)
		return time.Time{}, err
	}
	return created, nil
}

// Ping — health-check через `sys/health`. Не требует токена с правами на KV,
// поэтому пригоден и для bootstrap-проверок.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.c.Sys().HealthWithContext(ctx)
	if err != nil {
		return fmt.Errorf("vault: sys/health: %w", err)
	}
	return nil
}
