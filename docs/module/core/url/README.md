# core.url

Загрузка файла по URL (идея ansible `get_url`, переработанная под безопасный MVP).
**Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/url/url.go`](../../../../soul/internal/coremod/url/url.go) (контракт
и валидация) и [`soul/internal/coremod/url/fetched.go`](../../../../soul/internal/coremod/url/fetched.go)
(скачивание, verify, idempotency).

Модуль ориентирован на supply-chain-безопасность и работает по принципу
**secure-by-default + явный opt-out**: по умолчанию `https://`, SSRF-guard,
проверка TLS-цепочки и checksum-верификация **до** появления файла в целевом
пути — см. [Безопасность](#безопасность) ниже. Каждое из этих ограничений
оператор может снять отдельным флагом (`allow_http` / `allow_private` /
`insecure_skip_verify`), сняв ровно один контур; снятие → строка в output
`warnings` (оператор видит в результате apply). Без shell.

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `fetched` | Файл по адресу `url` материализован в `path` с заданными `mode`/`owner`/`group`. | См. таблицу веток ниже — поведение зависит от того, задан ли `checksum`. |

Поведение `fetched` по веткам:

| Условие | Действие | `changed` |
|---|---|---|
| `checksum` задан + файл существует и совпадает по хэшу | контент **не качается**; `mode`/`owner`/`group` приводятся к декларации (convergence) | `true` только если правился атрибут |
| `checksum` задан, файла нет / хэш не совпал | скачать во temp → verify по `checksum` → atomic rename; mismatch → `failed`, temp удаляется, целевой путь не трогается | `true` |
| `checksum` **не** задан + файл существует и совпал по SHA-256 | скачать во temp → сравнить SHA-256 с существующим → записи нет; `mode`/`owner`/`group` сверяются (convergence) | `true` только если правился атрибут |
| `checksum` **не** задан + содержимое отличается / файла нет | скачать во temp → atomic rename | `true` |

## fetched — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `url` | string | required | Адрес загрузки. По умолчанию **только `https://`** — `http://` и `file://` отвергаются в `Validate` (downgrade и чтение локальной ФС). `http://` допускается только при `allow_http: true`. |
| `path` | string | required | Целевой путь файла. Temp-файл скачивания создаётся в той же директории (для atomic rename). |
| `checksum` | string | optional | Ожидаемый хэш в форме `<algo>:<hex>`. Поддержаны `sha256` и `sha1` (`md5` сознательно **не** поддержан — слаб для supply-chain). Hex проверяется на длину и алфавит. Если задан — verify до публикации; если нет — idempotency идёт по SHA-256. |
| `mode` | string | optional | Права в octal-форме (`"0644"`, `"0755"`). При записи применяется к temp до rename; в no-op-ветке сверяется/правится только при заданном `mode` (пустой `mode` не навязывает дефолт существующему файлу). |
| `owner` | string | optional | Владелец (имя пользователя). Резолвится через `/etc/passwd`. |
| `group` | string | optional | Группа (имя). Резолвится через `/etc/group`. |
| `headers` | map | optional | HTTP-заголовки запроса (например `Authorization`). **Sensitive-by-construction** ([ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) §7.4): значения никогда не логируются и не попадают в output/register. `If-None-Match` / `If-Modified-Since` здесь дают условный GET — см. [304 conditional-GET](#304-conditional-get). |
| `timeout` | string | optional (default `300s`) | Таймаут запроса в `duration`-форме Soul Stack (`time.ParseDuration` + суффикс `<N>d`). Должен быть положительным. |
| `allow_http` | bool | optional (default `false`) | Снимает запрет `http://`: оператор разрешает plaintext-канал. **НЕ открывает SSRF** — dial-guard продолжает блокировать приватные адреса (отдельный флаг `allow_private`). Снятие → строка в output `warnings`. Кейс: внутренний artefact-mirror без TLS. |
| `insecure_skip_verify` | bool | optional (default `false`) | Отключает проверку TLS-цепочки (self-signed / internal CA). **MITM-риск** — взводится только осознанно. Снятие → строка в output `warnings`. Кейс: загрузка с internal-сервиса под собственным CA, не добавленным в системный trust store. |
| `allow_private` | bool | optional (default `false`) | Снимает SSRF-guard: разрешает dial в metadata / loopback / RFC1918 / link-local. Снятие → строка в output `warnings`. Кейс: легитимный internal endpoint (внутренний repo-зеркало в RFC1918). |

## 304 conditional-GET

Если оператор передаёт в `headers` валидатор кэша (`If-None-Match: "<etag>"` или
`If-Modified-Since: <date>`), сервер может ответить **304 Not Modified** — тело не
передаётся. Модуль трактует это штатно, отдельного param не вводится (валидатор
кладётся через обычные `headers`):

| Ситуация | Поведение |
|---|---|
| 304 + локальный файл по `path` **существует** | контент актуален → no-op: `mode`/`owner`/`group` приводятся к декларации (convergence), `output.sha256`/`size` — по существующему файлу. `changed=true` только если правился атрибут. |
| 304 + локального файла **нет** | **`failed`** (fail-fast): сервер прислал «не изменилось», но кэша нет — это stale `If-None-Match` без локальной копии. Скачать нечего, тело 304 не несёт payload. Сообщение: `server returned 304 but no local file at <path>: stale If-None-Match without cache`. |

Работает и по `https://`, и по `http://` (при `allow_http`). 304 проверяется
**раньше** общей проверки 2xx, поэтому не попадает в `unexpected status`.

## Безопасность

Модуль — основная supply-chain-граница загрузки файлов, поэтому по умолчанию
ограничения жёсткие. Это **secure-by-default**: каждый контур снимается отдельным
opt-out-флагом (см. таблицу params), флаги ортогональны, снятие любого кладёт
строку в output `warnings` (оператор видит в результате apply; только `host`, без
полного URL и без `headers` — query/path и заголовки могут нести секреты).
([ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)
«безопасность на первом месте»):

- **По умолчанию только `https://`** (`util.ValidateFetchURL(url, allow_http)`).
  Схема сверяется через `url.Parse` регистронезависимо и по-настоящему —
  `http://`, `file://` и трюки вида `https://\nhttp://evil` отвергаются.
  `allow_http: true` допускает `http://`, но `file://`/`ftp://` остаются
  запрещены, и SSRF-guard не ослабляется.
- **SSRF-guard.** Dial в metadata (`169.254.169.254`), loopback, RFC1918,
  link-local/CGNAT/site-local заблокирован по **фактически резолвнутому IP**. Это
  закрывает прямой SSRF (кража cloud-metadata IAM-кредов) и DNS-rebind (dial идёт
  по уже проверенному IP, без второго резолва имени). Хост с парой A-записей
  «публичный + metadata» блокируется целиком. Снимается **только** флагом
  `allow_private: true` (легитимный internal endpoint); `allow_http` его не
  трогает.
- **TLS — системный trust store** по умолчанию. Проверка цепочки снимается
  флагом `insecure_skip_verify: true` (self-signed / internal CA, MITM-риск).
- **Редиректы**: `CheckRedirect` отвергает downgrade `https→http`. При
  `allow_http: true` downgrade-hop допускается (парно со снятием https-only), но
  редирект на не-`http(s)` схему по-прежнему блокируется, как и dial в приватные
  адреса на hop'е (если не снят `allow_private`).
- **Checksum verify ДО публикации.** Скачивание всегда идёт во временный файл в
  директории `path`, хэш считается на лету. Verify против `checksum` происходит до
  `rename`: при mismatch temp удаляется, целевой путь не трогается — **неверный
  хэш не материализуется никогда**.
- **`headers` не раскрываются** нигде: ни в логах, ни в output/register, ни в
  эхо-поле `url`.

## Capabilities / side-effects

- **Сетевой доступ:** HTTPS GET по `url` (с SSRF-guard).
- **Меняет файловую систему:** создаёт/перезаписывает `path` через atomic rename
  из temp-файла; правит mode и владельца. Для системных путей требует прав.
- **Не выполняет подпроцессов** — скачивание, хэширование и запись in-process,
  без shell.

## Output / register

`{ path, url, sha256, size, changed, fetched: true }`, где `sha256` — SHA-256
записанного/совпавшего содержимого (всегда SHA-256 для output, даже если
`checksum` задавался по sha1), `size` — размер в байтах, `url` — эхо без `headers`.

## Пример

`fetched` с checksum-верификацией — скачать релиз с GitHub Releases:

```yaml
- name: Fetch node_exporter tarball
  module: core.url.fetched
  params:
    url: "${ 'https://github.com/prometheus/node_exporter/releases/download/v' + input.version + '/node_exporter-' + input.version + '.linux-' + input.arch + '.tar.gz' }"
    path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
    checksum: "${ input.sha256 }"
    mode: "0644"
```

(из [`examples/destiny/destiny-node-exporter/tasks/main.yml`](../../../../examples/destiny/destiny-node-exporter/tasks/main.yml) —
тройка «скачать → распаковать → разложить бинарь»)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-010](../../../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов) — sensitive-by-construction `headers`.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — «безопасность на первом месте».
