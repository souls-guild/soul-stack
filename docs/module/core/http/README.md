# core.http

Read-probe HTTP-эндпоинта (health-check / API-readiness / чтение версии).
**Soul-side**, статически встроен в `soul`-бинарь. Реализация —
[`soul/internal/coremod/http/http.go`](../../../../soul/internal/coremod/http/http.go)
(диспетчер + валидация params) и
[`soul/internal/coremod/http/probe.go`](../../../../soul/internal/coremod/http/probe.go)
(verb `probe`). Идея заимствована из Ansible `uri`, но сознательно сужена до
**чтения**: «делаем хорошо» вместо мутной вседозволенности.

`probe` — это **verb-форма**, а не declarative-state: он ничего не приводит к
состоянию, а возвращает факты об эндпоинте в `register`. Мутирующие HTTP
(POST/PUT/PATCH/DELETE) сознательно отложены post-MVP отдельным ADR-расширением
(вероятно `core.http.request`) — тогда же будет решён changed-контракт для
мутаций. `probe` остаётся строго read-only.

## Read-only: ничего не меняет

`changed = false` **всегда**, конструктивно и ненастраиваемо — read-probe не
меняет состояние хоста. Прецедент — `core.exec.run`: модуль даёт факты, а
интерпретирует их `changed_when:` на уровне scenario. Идемпотентность —
по природе (no-op для состояния).

## States

| State (verb) | Назначение | `changed` |
|---|---|---|
| `probe` | Один GET/HEAD-запрос к `url`; ответ (status / body / elapsed_ms / headers_keys) возвращается в `register`. | `false` всегда (read-only). |

## probe — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `url` | string | required | Целевой URL. **По умолчанию только `https://`** (снять до `http(s)` — `allow_http`; `file://` запрещён всегда). См. «Безопасность». |
| `method` | string | optional (default `GET`) | HTTP-метод. Разрешены только read-only `GET` / `HEAD` (сравнение регистронезависимо, `get` → `GET`). Мутирующие методы отвергаются на `Validate`. `HEAD` не читает тело. |
| `headers` | map | optional | Заголовки запроса. **Sensitive-by-construction** ([ADR-010 §7.4](../../../templating.md)): значения никогда не логируются и не попадают в output — в output отдаётся только список **ключей** (`headers_keys`). |
| `status_codes` | list of int | optional (default `[200]`) | Набор ожидаемых статус-кодов. Фактический статус вне набора → шаг `failed` (но с приложенным output для диагностики). |
| `timeout` | string (duration) | optional (default `30s`) | Таймаут запроса в convention `duration` Soul Stack (`time.ParseDuration` + суффикс `<N>d`). Должен быть положительным. Короче, чем у `core.url` (health-check, не download). |
| `allow_private` | bool | optional (default `false`) | Снимает **SSRF-guard** для легитимного internal health-check (см. «Безопасность»). |
| `allow_http` | bool | optional (default `false`) | Снимает **https-only**: допускает `http://` (downgrade-редирект https→http тоже разрешается). `file://` остаётся запрещён. **Не** открывает SSRF — dial-guard живёт отдельно (ортогонально `allow_private`). |
| `insecure_skip_verify` | bool | optional (default `false`) | Отключает **TLS-верификацию** (self-signed / internal CA). MITM-риск — взводить только для доверенного internal-эндпоинта. Ортогонально прочим флагам. |

Все три флага — **secure-by-default + явный opt-out**: каждый ослабляет отдельный
независимый контур, снятие одного не затрагивает другие. При взведении любого
из них probe кладёт в output поле `warnings` (см. «Output / register»).

## Безопасность

[ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) «безопасность на первом месте»:

Все контуры **secure-by-default**; каждый снимается отдельным явным
opt-out-param-ом, и снятие одного **не** ослабляет другие (флаги ортогональны).

- **https-only** (default) — `http://` и `file://` отвергаются
  (`util.ValidateFetchURL`). Снять до `http(s)` — `allow_http: true` (`file://`
  остаётся запрещён даже с `allow_http`).
- **TLS** — системный trust store (default). Отключить верификацию для
  self-signed / internal CA — `insecure_skip_verify: true`. MITM-риск:
  взводить только для доверенного internal-эндпоинта.
- **Downgrade-защита редиректов** (default) — редирект на не-https блокируется.
  При `allow_http: true` downgrade-hop `https→http` допускается (парно с
  разрешённым http); редирект на не-`http(s)` схему блокируется всегда.
- **SSRF-guard** (default) — probe в metadata / loopback / RFC1918 / link-local
  заблокирован по фактически резолвнутому IP (закрывает прямой SSRF на
  cloud-metadata IAM `169.254.169.254` и DNS-rebind). Снять для легитимного
  internal health-check — `allow_private: true`. **`allow_http` SSRF не
  открывает** — dial-guard живёт отдельно (http по-прежнему не дойдёт до
  metadata/loopback без `allow_private`).
- **Warning при снятии guard** — при взведении любого opt-out-флага probe кладёт
  в output поле `warnings` (по одной строке на снятый контур): оператор видит
  факт ослабления security. В warning попадает только **host** (НЕ полный URL —
  он может нести `path`/`query` с sensitive-данными, и НЕ headers). Формулировки:
  `TLS verification disabled (insecure_skip_verify) for <host>` /
  `plaintext http allowed (allow_http) for <host>` /
  `SSRF-guard disabled (allow_private) for <host>`.
- **Headers** — sensitive-by-construction (значения не логируются, в output —
  только ключи).
- **Cap тела** — ответ читается не более `64 KiB` (защита от OOM); сверх лимита
  тело отбрасывается, в output ставится `truncated: true` (граница режется по
  полной UTF-8-руне).

## Capabilities / side-effects

- **Не выполняет подпроцессов.** Чистый HTTP-клиент в памяти
  ([`probe.go`](../../../../soul/internal/coremod/http/probe.go) — `doer.Do`); в
  манифесте ([`http.yaml`](../../../../shared/coremanifest/http.yaml)) объявлен
  только [`network_outbound`](../../../naming-rules.md#required_capabilities-enum)
  (исходящий запрос), **без** `exec_subprocess` и `fs_write_root`.
- **Read-only, ничего не пишет.** В отличие от [`core.url`](../url/README.md) (тот
  скачивает файл и пишет на FS → объявляет `fs_write_root`), `probe` не трогает
  файловую систему вообще: ответ читается в память и возвращается в `register`.
  `changed = false` конструктивно (см. «Read-only: ничего не меняет»).
- **Cap тела — `64 KiB`** (`maxBodyBytes`,
  [`probe.go`](../../../../soul/internal/coremod/http/probe.go)): защита от OOM на
  большом ответе. Сверх лимита тело отбрасывается (`truncated: true`), граница
  режется по полной UTF-8-руне.
- **SSRF-guard, downgrade- и TLS-защита** на сетевом вызове (см. «Безопасность»):
  default-клиент блокирует metadata/loopback/RFC1918/link-local по резолвнутому IP
  (`allow_private`), отвергает downgrade-редирект (`allow_http`) и верифицирует
  TLS-цепочку (`insecure_skip_verify`). HTTP-клиент строится **per-call** из этих
  трёх ортогональных флагов (`util.NewHTTPClient(util.HTTPClientOpts{…})`), а не
  выбирается из пред-собранных инстанций.

## Output / register

`{ status, body, truncated, elapsed_ms, changed: false }`; `headers_keys`
добавляется, только если были заданы `headers` (отсортированный список ключей,
без значений). `warnings` (список строк) добавляется, только если был взведён
хотя бы один opt-out-флаг (`allow_private` / `allow_http` / `insecure_skip_verify`)
— по одной строке на снятый контур, с `host` (без полного URL и headers).

Тело (`body`) отдаётся как есть — sensitive-целиком оно **не** считается
(health-эндпоинт штатно возвращает `{"status":"ok"}`, ради этого probe и нужен).
Из тела маскируются **только** vault-ref-подстроки (`vault:…` → `***MASKED***`),
включая ref внутри JSON. Произвольный plaintext-секрет в теле **не** маскируется:
тело semi-trusted, и оператор не должен класть в probe-эндпоинт то, что не должно
светиться. Бинарные / битые байты тела приводятся к валидному UTF-8 (замена на
U+FFFD), чтобы probe возвращал чистый результат, а не ронял шаг.

При статусе вне `status_codes` шаг — `failed`, но с тем же output (фактический
status/body нужны для диагностики). Транспортная ошибка (DNS/TLS/timeout/
заблокированный downgrade-редирект) → `failed` без output.

## Пример

```yaml
- name: Wait until the service answers HTTP 200
  module: core.http.probe
  register: health
  retry: { count: 5, delay: 3s }
  params:
    url: https://service.internal:8443/healthz
    method: GET
    status_codes: [200]
    allow_private: true
```

(минимальный валидный пример — в `examples/` задач для `core.http` пока нет)

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/url/README.md](../url/README.md) — загрузка файла по URL (`fetched`, тоже https-only).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
- [ADR-016](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack) — «безопасность на первом месте» (https-only, SSRF-guard).
- [templating.md](../../../templating.md) — секрет-маскинг и sensitive-by-construction (§7.4).
