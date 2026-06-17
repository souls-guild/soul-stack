# Прод-конвенции destiny

Чек-лист «прод-grade destiny» — что отличает эталонную production-ready destiny от черновой. Это **нормативные** правила для destiny, которые ставят и держат системный сервис на хосте (демоны под systemd: exporter-ы, redis, postgres и т.п.). Источник эталона — [`examples/destiny/destiny-node-exporter/`](../../examples/destiny/destiny-node-exporter/); остальные сервис-destiny приводятся к этому же паттерну.

Опираются на [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (изоляция destiny), [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) (набор core-модулей) и инвариант [«безопасность на первом месте»](../requirements.md) из requirements.md.

## 1. Passthrough флагов и конфига

Не зашивай в destiny закрытый список флагов демона. Объяви `extra_args` (`type: array`, `items: { type: string }`, `default: []`) как сквозной канал для любых флагов сверх базовых.

- Передаётся в шаблон нативным списком: вся ячейка `vars` = один `${ input.extra_args }` → по правилу ADR-010 «non-string CEL-результат, ячейка целиком = `${…}`» приходит реальный список, не строка.
- В `.tmpl` разворачивается `range`-ом, каждый элемент — отдельный токен `ExecStart`, не разбивается по пробелам:

  ```gotemplate
  ExecStart={{ .vars.bin_dir }}/node_exporter --web.listen-address={{ .vars.listen }}{{ range .vars.extra_args }} {{ . }}{{ end }}
  ```

- Значение-с-пробелом передаётся одним элементом списка: `["--web.config.file=/etc/x.yml"]`, а не `["--web.config.file", "/etc/x.yml"]` через шаблонную склейку.

Это даёт оператору расширять поведение демона (коллекторы, textfile-dir, TLS-конфиг) без правки самой destiny.

## 2. Сервис-аккаунт — гибрид-правило

**Нормативное правило**, как сервис получает непривилегированный аккаунт. Выбор определяется тем, владеет ли демон стабильным состоянием на диске:

| Тип сервиса | Признак | Аккаунт |
|---|---|---|
| **Stateless-демон** | нет owned data-dir, ничего не пишет под фиксированным uid (exporter-ы, прокси без локального состояния) | `DynamicUser=yes` в unit-е. systemd сам заводит запертого transient-юзера на время жизни процесса. Ручные `core.user`/`core.group` **НЕ заводятся.** |
| **Stateful-сервис** | владеет каталогом данных, нужен стабильный uid для прав на файлы между рестартами и апгрейдами (redis, postgres) | Ручной system-uid через `core.user.present`/`core.group.present` (no-login shell, без home, gid/uid не пинуются — системный диапазон). На него ссылается `User=`/`Group=` в unit-е. |

Почему именно так:

- Для stateless `DynamicUser=yes` строго безопаснее ручного аккаунта — uid эфемерный, между перезапусками не переиспользуется, юзер не висит в `/etc/passwd`, нечего захватывать. Ручной аккаунт здесь — лишняя поверхность без выгоды.
- Для stateful `DynamicUser` непригоден: файлы данных, созданные под transient-uid одного запуска, на следующем запуске принадлежат «чужому» uid. Тут нужен стабильный аккаунт.

Не смешивай: stateless-destiny с `DynamicUser` не должна заводить ручного юзера «на всякий случай», stateful не должна полагаться на `DynamicUser` для data-dir.

## 3. systemd-hardening — обязателен во всех unit-шаблонах

Каждый рендеримый systemd-unit несёт блок hardening. Это прямое следствие инварианта «безопасность на первом месте» ([requirements.md](../requirements.md)); на момент введения конвенции ни один пример его не имел — это закрываемый пробел, а не «опционально».

Базовый блок (статический текст шаблона, в `render_context` не попадает):

```ini
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
CapabilityBoundingSet=
```

Правила настройки под конкретный демон:

- `RestrictAddressFamilies` — добавляй `AF_UNIX` только если демон реально слушает/использует unix-сокет. Чисто TCP-демон (node_exporter) обходится `AF_INET AF_INET6`.
- `CapabilityBoundingSet=` (пусто) — для демонов, которым не нужны capabilities (exporter-ы читают публичные метрики ядра/ФС). Если демону действительно нужна capability (напр. `CAP_NET_BIND_SERVICE` для порта <1024) — перечисли её явно, не открывай весь набор.
- `ProtectSystem=strict` делает весь `/` только-чтение. Если демон лишь читает (`/proc`, `/sys`) — этого достаточно, `ReadWritePaths` не нужен. Добавляй `ReadWritePaths=<dir>` **только** под фактический каталог записи (data-dir stateful-сервиса), и только его — не расширяй «на всякий случай».

### 3a. Stateful-вариант: ReadWritePaths + drop-in (redis)

Базовый блок выше — для stateless-демона со своим рендеримым unit-ом (node_exporter, `DynamicUser`). Stateful-сервис, который ставится **дистрибутивным пакетом** (redis, postgres), отличается двумя вещами:

- **Hardening доезжает drop-in-ом, а не своим unit-ом.** Дистрибутивный unit заменять целиком нельзя — он несёт `ExecStart`/`Type=notify`/`RuntimeDirectory` и обновляется вместе с пакетом. Override кладётся отдельной задачей `core.file.rendered` в `/etc/systemd/system/<unit>.service.d/hardening.conf` (`mode 0644`, `owner/group root` — это файл systemd, не сервис-аккаунта). Директивы `[Service]` мержатся поверх дистрибутивных, наши имеют приоритет.
- **`ProtectSystem=strict` обязан нести `ReadWritePaths`** под все каталоги, куда сервис пишет, иначе strict сделает их только-чтение и сломает запись — это **прод-инцидент**, а не «строже». Пути берутся из конфига сервиса (для redis — `dir`/`unixsocket`/`pidfile`/`logfile` в `redis.conf`); в `ReadWritePaths` перечисляются их **каталоги**, и только они.

Пример drop-in для redis (`redis-server.service.d/hardening.conf`):

```ini
[Service]
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/redis /var/run/redis /var/log/redis
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
RestrictNamespaces=yes
CapabilityBoundingSet=
```

Отличия от stateless-блока и их причины:

- **`User=`/`Group=` в drop-in НЕ задаются.** Аккаунт — от пакета: дистрибутивный пакет заводит system-аккаунт (`redis:redis`) со стабильным uid, и дистрибутивный unit уже запускается под ним. Это и есть stateful-путь §2 (стабильный uid-владелец data-dir между рестартами/апгрейдами), `DynamicUser` тут непригоден. Ручные `core.user`/`core.group` для аккаунта от пакета не нужны.
- **`MemoryDenyWriteExecute` НЕ ставится** (в отличие от Go-бинарей exporter-ов, где он уместен). redis — на C, использует jemalloc и поддерживает loadable-модули; MDWE запрещает W+X-маппинги и способен ломать аллокатор/JIT-страницы модулей. Для C-сервиса с таким профилем MDWE не добавляй.
- **`RestrictAddressFamilies` несёт `AF_UNIX`** — redis слушает И TCP, И unix-сокет (дополни список под фактические семейства сервиса, §3 правило).

**daemon-reload + restart на изменение drop-in.** Drop-in меняет уже загруженный unit, поэтому одного `core.service.restarted` мало — systemd обязан сперва перечитать конфигурацию. Цепочка реактивная и идемпотентная:

1. `core.file.rendered` drop-in (`register: <hardening>`).
2. `core.exec.run systemctl daemon-reload` с `onchanges: [<hardening>]` — перечитать unit до рестарта.
3. `core.service.restarted` с `onchanges: [<config>, <hardening>]` — рестарт при изменении конфига сервиса **или** drop-in (изменённый drop-in вступает в силу только после рестарта).

Порядок гарантируется позицией задач (drop-in → daemon-reload → restart). Ничего не изменилось → вся цепочка no-op.

## 4. TLS / web-config — через `extra_args`, не зашивать

TLS и basic-auth демонов семейства Prometheus включаются флагом `--web.config.file=<path>`. Передавай этот флаг через `extra_args` (п. 1), сам `web.yml` (сертификаты, хэши паролей) — **отдельная задача или отдельная destiny**, не часть destiny демона.

Причина: web-config — секрет-несущий артефакт с собственным жизненным циклом (ротация сертификатов независима от версии демона). Зашивать его рендер в destiny демона связывает два независимых цикла и тащит секреты в чужую зону ответственности.

## 5. arch / os — из soulprint у caller-а

Архитектура (и любой os-факт) приходит в destiny через `apply: input:` от caller-а, а не определяется внутри destiny:

```yaml
apply:
  destiny: node-exporter
  input:
    arch: "${ soulprint.self.os.arch }"
```

- destiny изолирована ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)) и `soulprint.*` напрямую не видит — факт доезжает только через `apply: input:`.
- `default` для `arch` в самой destiny — **fallback** для прямых вызовов, не основной путь. Один incarnation может смешивать amd64/arm64-хосты, поэтому реальная архитектура каждого хоста обязана прийти из soulprint, иначе tarball не совпадёт с платформой.
- soulprint уже отдаёт значение в нотации релиза (`amd64`/`arm64`, см. [`docs/soul/soulprint.md`](../soul/soulprint.md)) — маппинг не нужен.

## 6. Идемпотентность каждого шага

Каждая задача destiny — no-op при повторном apply с теми же входами (инвариант destiny, [concept.md](concept.md)). На практике у прод-шага есть явный признак «уже сделано»:

- `core.url.fetched` / `core.file.rendered` — checksum/содержимое совпало → no-op.
- `core.archive.extracted` — marker распакованного архива.
- `core.cmd.shell` / `core.exec.run` — `creates:` (путь-результат уже на месте → не выполнять).
- `core.pkg.installed` / `core.service.running` — декларативная природа модуля (`present`/`running`).
- Рестарт — только реактивно: `core.service.restarted` с `onchanges: [<register unit-задачи>]`, а не безусловно.

Шаг без признака идемпотентности (особенно `cmd`/`exec` без `creates:`) — баг прод-grade destiny.

## 7. Supply-chain

Любой артефакт, скачиваемый из сети:

- **`checksum` обязателен и fail-closed.** В `input:`-контракте поле хэша — `required: true` **без `default`**. Нет хэша → честный отказ destiny, а не fetch с placeholder-ом. `core.url.fetched` верифицирует хэш **до** материализации файла — неверный хэш не попадает на диск.
- **https-only.** URL артефакта — только `https://`. Никаких `http://` для скачивания бинарей/архивов.
- Хэш привязан к паре `(version, arch)` — берётся из официального `sha256sums` релиза под конкретный tarball.

## 8. Изоляция destiny (ADR-009)

destiny видит **только свой `input:`**. Никакого чтения чужого контекста:

- Нет доступа к `incarnation.state`, к фактам других хостов, к `essence` сервиса, к scenario-scope.
- Cross-host и cloud/vault-данные приходят исключительно через `apply: input:` от caller-а (scenario резолвит `soulprint.where(...)`, `vault(...)`, `essence.*` на своей стороне и передаёт уже значения).
- Результат наружу — только через объявленный top-level `output:` ([output.md](output.md)), не через подглядывание.

Это инвариант, а не рекомендация: он держит destiny переиспользуемой и независимо тестируемой.

## См. также

- [concept.md](concept.md) — что такое destiny, её инварианты (атомарность/декларативность/идемпотентность/изоляция).
- [tasks.md](tasks.md) — формат задач, `onchanges`/`register`/`creates`/`retry`.
- [input.md](input.md) — `input:`-контракт (где `required`/`default`/`pattern` валидируются).
- [output.md](output.md) — как destiny отдаёт результат caller-у.
- [`docs/templating.md`](../templating.md) — CEL + Go text/template, `${…}`-маркер, `core.file.rendered`, правило non-string CEL-ячейки.
- [`docs/soul/soulprint.md`](../soul/soulprint.md) — `soulprint.self.os.arch` и прочие факты хоста.
- [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — набор core-модулей MVP (`core.url`/`core.archive`/`core.cmd`/`core.file`/`core.service`/`core.user`/`core.group`).
- [`examples/destiny/destiny-node-exporter/`](../../examples/destiny/destiny-node-exporter/) — эталон, по которому собрана конвенция (stateless + DynamicUser + hardening + extra_args).
