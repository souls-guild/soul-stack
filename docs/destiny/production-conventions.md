# Прод-конвенции destiny

Чек-лист «прод-grade destiny» — что отличает эталонную production-ready destiny от черновой. Это **нормативные** правила для destiny, которые ставят и держат системный сервис на хосте (демоны под systemd: exporter-ы, redis, postgres и т.п.). Источник эталона — [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/); остальные сервис-destiny приводятся к этому же паттерну.

**Эталон один — `node-exporter` (stateful-ветка).** В `examples/destiny/node-exporter/` лежит generic-destiny Prometheus node_exporter: бинарь, юзер, группа и unit называются `node_exporter`, демон работает под **ручным стабильным system-аккаунтом `node_exporter`** (stateful-ветка §2 — textfile-каталог железных метрик переживает рестарты и читается root-сборщиками, нужен стабильный uid-владелец), несёт systemd-hardening (§3), **version-aware install** (`unless --version` → апгрейд бинаря рестартит сервис, §6), опциональный `checksum` под зеркала (§7) и **привилегированные textfile-коллекторы** smartmon / nvme / ipmi отдельными oneshot-таймерами под жёстким systemd-sandbox (§3b). Имя «node-exporter» в `apply: { destiny: node-exporter }` ниже относится именно к этому эталону.

> **DynamicUser остаётся валидным выбором** для **stateless**-демонов (§2, stateless-ветка) — просто текущий эталон его не иллюстрирует, т.к. node-exporter stateful (textfile-каталог, см. §2). Правило выбора аккаунта (DynamicUser vs ручной uid) — нормативное и не зависит от того, какой пример его показывает.

Опираются на [ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (изоляция destiny), [ADR-015](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) (набор core-модулей) и инвариант [«безопасность на первом месте»](../requirements.md) из requirements.md.

## 1. Passthrough флагов и конфига

Не зашивай в destiny закрытый список флагов демона. Объяви `extra_args` (`type: array`, `items: { type: string }`, `default: []`) как сквозной канал для любых флагов сверх базовых.

- Передаётся в шаблон нативным списком: вся ячейка `vars` = один `${ input.extra_args }` → по правилу ADR-010 «non-string CEL-результат, ячейка целиком = `${…}`» приходит реальный список, не строка.
- В `.tmpl` разворачивается `range`-ом, каждый элемент — отдельный токен `ExecStart`, не разбивается по пробелам:

  ```gotemplate
  ExecStart={{ .vars.bin_dir }}/<daemon> --web.listen-address={{ .vars.listen }}{{ range .vars.extra_args }} {{ . }}{{ end }}
  ```

- Значение-с-пробелом передаётся одним элементом списка: `["--web.config.file=/etc/x.yml"]`, а не `["--web.config.file", "/etc/x.yml"]` через шаблонную склейку.

Это даёт оператору расширять поведение демона (коллекторы, textfile-dir, TLS-конфиг) без правки самой destiny. Рабочая иллюстрация — `redis_exporter_extra_args` в инлайн-блоке `redis_exporter` сценария [`monitoring/scenario/create`](../../examples/service/monitoring/scenario/create/main.yml) (уходит в `redis_exporter.service.tmpl` через `range`). Эталонный `node-exporter` сам `extra_args` не объявляет — он вместо этого выносит конкретные настройки в именованные `input`-параметры (`listen`/`collectors`/`bin_dir`/`user`/`textfile_dir`/…); оба подхода валидны, выбор — между «открытый канал любых флагов» и «явный типизированный контракт».

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

Иллюстрации обеих веток:

- **Stateful + ручной system-аккаунт** — эталонный [`node-exporter/`](../../examples/destiny/node-exporter/): `core.group.present` + `core.user.present` (`system: true`, `shell: /usr/sbin/nologin`, `home: /`) заводят `node_exporter`, на который ссылается `User=`/`Group=` в unit-е. Стабильный uid нужен, т.к. textfile-каталог `--collector.textfile.directory` (`/var/lib/node_exporter`) владеется этим аккаунтом и переживает рестарты, а в него пишут привилегированные oneshot-сборщики (§3b); группа создаётся **до** пользователя (`core.user -g` требует существующую primary-группу). Это не «аккаунт от дистрибутивного пакета» (§3a, redis), а явно заводимый destiny system-аккаунт — допустимый stateful-путь, когда сервис ставится не пакетом, а из tarball-релиза.
- **Stateless + `DynamicUser`** — отдельного destiny-примера в репозитории сейчас нет, но ветка остаётся нормативной: stateless-демон без owned data-dir (exporter без textfile-каталога, прокси без локального состояния) получает аккаунт от самого systemd через `DynamicUser=yes` в unit-е, ручные `core.user`/`core.group` не заводятся.

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

Базовый блок выше — для демона со **своим рендеримым unit-ом** (node-exporter ставится из tarball-релиза и рендерит unit целиком). Stateful-сервис, который ставится **дистрибутивным пакетом** (redis, postgres), отличается двумя вещами:

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

**daemon-reload + restart на изменение drop-in.** Drop-in меняет уже загруженный unit, поэтому одного `core.service.restarted` мало — systemd обязан сперва перечитать конфигурацию.

> **С версии с централизованным daemon-reload в `core.service` ([ADR-015 Amendment 2026-06-18](../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список)) отдельный шаг `core.exec.run systemctl daemon-reload` для рестарта стал НЕОБЯЗАТЕЛЬНЫМ.** `core.service.restarted`/`running`/`enabled` по умолчанию (`daemon_reload: auto`) сами проверяют systemd-флаг `NeedDaemonReload` и перечитывают unit перед action — изменённый drop-in уже учтён. **Предпочтительно полагаться на встроенный `auto`** и не дублировать reload отдельной задачей. См. [docs/module/core/service/README.md → daemon_reload](../module/core/service/README.md#daemon_reload--перечитывание-unit-файлов).

Ручной паттерн ниже остаётся валидным (например когда reload нужен явно вне привязки к рестарту сервиса, или для исторических destiny). Реактивная и идемпотентная цепочка:

1. `core.file.rendered` drop-in (`register: <hardening>`).
2. `core.exec.run systemctl daemon-reload` с `onchanges: [<hardening>]` — перечитать unit до рестарта.
3. `core.service.restarted` с `onchanges: [<config>, <hardening>]` — рестарт при изменении конфига сервиса **или** drop-in (изменённый drop-in вступает в силу только после рестарта).

Порядок гарантируется позицией задач (drop-in → daemon-reload → restart). Ничего не изменилось → вся цепочка no-op. С опорой на встроенный `auto` шаг 2 опускается: `core.service.restarted` (`daemon_reload: auto`) делает reload сам перед рестартом.

### 3b. Привилегированный oneshot-коллектор: root + узкий sandbox + Condition-gate

Бывает, что вспомогательная задача требует **больше** привилегий, чем основной демон, — например textfile-коллектор железных метрик читает блочные устройства, IPMI или NVMe (доступно только root). Прямолинейное «дать root всему сервису» противоречит §3. Правильный паттерн (иллюстрация — коллекторы smartmon / nvme / ipmi в [`node-exporter/`](../../examples/destiny/node-exporter/)): вынести привилегированную работу в **отдельный oneshot `.service` + `.timer`**, а не вешать её на основной демон.

- **Основной демон остаётся беспривилегированным.** Он только **читает** готовые `.prom` из textfile-каталога (`--collector.textfile`), сами метрики пишут oneshot-сборщики. В его unit-е каталог — `ReadOnlyPaths=<textfile_dir>` (не RW): демон туда не пишет.
- **Oneshot — `Type=oneshot`, `User=root`, но sandbox максимально узкий под фактическую нужду.** Root даётся не «вообще», а сужается: `CapabilityBoundingSet=` оставляет **только** реально нужные capabilities (smartmon — `CAP_SYS_RAWIO CAP_SYS_ADMIN` под RAW IO к дискам), `DevicePolicy=closed` + точечный `DeviceAllow=block-* r` открывает **только** нужный класс устройств, `PrivateNetwork=yes` (сборщику сеть не нужна), `ReadWritePaths=<textfile_dir>` — единственный каталог записи. Остальной hardening-блок (`ProtectSystem=strict`, `ProtectKernel*`, `RestrictSUIDSGID`, `NoNewPrivileges` и т.д.) — как в §3.
- **Запись `.prom` атомарна:** сборщик пишет во временный файл и `mv`-ит в финальный (`mktemp … > tmp; mv tmp <dir>/<name>.prom`), чтобы node_exporter не прочитал полузаписанный файл.
- **Condition-gate против старта без железа.** Каждый коллектор-unit (и его `.timer`) несёт `Condition*`, не дающий ему стартовать там, где железа нет: smartmon — `ConditionVirtualization=no` (на VM физических дисков нет), nvme — `ConditionPathExistsGlob=/dev/nvme[0-9]*`, ipmi — `ConditionPathExists=/dev/ipmi0`. Одинаковая `Condition*` ставится и в `.service`, и в его `.timer` (защита и от планового, и от ручного `start`). Благодаря этому **установка** коллектора безопасна и на VM: задачи destiny раскладывают unit/timer, но systemd просто не активирует их без соответствующего устройства. Это снимает нужду в per-host-логике «ставить коллектор или нет» внутри destiny — гейтинг отдаётся systemd.
- **Опциональность коллекторов — через `input:`-список + `when:`.** Какие коллекторы устанавливать, выбирается `input.collectors` (`type: array`, `items: { enum: [smartmon, nvme, ipmi] }`, дефолт destiny — полный набор `[smartmon, nvme, ipmi]`); каждая задача коллектора несёт `when: "'<name>' in input.collectors"` (голый CEL, top-level expression-ключ — без `${…}`). Два уровня независимы: `when` решает, **ставить ли** артефакты коллектора вообще; `Condition*` решает, **стартует ли** поставленный коллектор на этом железе. На service-уровне этот выбор пробрасывается одноимённым по смыслу параметром (в `monitoring` — `node_exporter_collectors`, `array<string>` со значениями `smartmon`/`nvme`/`ipmi`, default `[]`: на VM железа нет → только ядро node_exporter) и уходит в destiny через `apply: input: { collectors: ${ input.node_exporter_collectors } }`.

## 4. TLS / web-config — через `extra_args`, не зашивать

TLS и basic-auth демонов семейства Prometheus включаются флагом `--web.config.file=<path>`. Передавай этот флаг через `extra_args` (п. 1), сам `web.yml` (сертификаты, хэши паролей) — **отдельная задача или отдельная destiny**, не часть destiny демона.

Причина: web-config — секрет-несущий артефакт с собственным жизненным циклом (ротация сертификатов независима от версии демона). Зашивать его рендер в destiny демона связывает два независимых цикла и тащит секреты в чужую зону ответственности.

## 5. arch / os — из `soulprint.self`

Архитектура (и любой стабильный os-факт целевого хоста) читается destiny **напрямую** из `soulprint.self.os.arch`. После amendment 2026-06-18 ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) / [ADR-010](../adr/0010-templating.md), см. [`docs/templating.md`](../templating.md)) стабильный self-слой `soulprint.self.*` доступен и в CEL-проходе destiny — это per-host свойство, а не scenario-scope. Эталонный `node-exporter` так и делает: в `input:` поля `arch` **нет**, URL tarball-а собирается прямо из факта:

```yaml
# tasks/install.yml эталона node-exporter
url: "${ input.base_url + '/v' + input.version + '/node_exporter-' + input.version + '.linux-' + soulprint.self.os.arch + '.tar.gz' }"
```

- Граница изоляции destiny ([ADR-009](../adr/0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация), §8) проходит по **self vs топология прогона**: `soulprint.self.*` (стабильный факт текущего хоста) — доступен, а cross-host `soulprint.hosts`/`soulprint.where(...)` — scenario-only, в destiny отсекаются. Один incarnation может смешивать amd64/arm64-хосты — каждый хост получает свой `soulprint.self.os.arch`, поэтому отдельный `input: arch` не нужен (он сделал бы архитектуру одинаковой на весь прогон).
- `apply: input:` остаётся каналом для значений, которые destiny **не** может вывести из своего self-слоя: caller-производные данные, cross-host факты (`soulprint.where(...)`), `vault(...)`/`essence.*` — их scenario резолвит у себя и передаёт готовыми.
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

- **`checksum` обязателен и fail-closed (нормативный default).** В `input:`-контракте поле хэша — `required: true` **без `default`**. Нет хэша → честный отказ, а не fetch с placeholder-ом. `core.url.fetched` верифицирует хэш **до** материализации файла — неверный хэш не попадает на диск. Так сделано на **service-уровне**: сценарий [`monitoring`](../../examples/service/monitoring/) объявляет и `node_exporter_sha256`, и `redis_exporter_sha256` как `required: true` без default и прокидывает значение в destiny через `apply: input:`.
- **https-only.** URL артефакта — только `https://`. Никаких `http://` для скачивания бинарей/архивов.
- Хэш привязан к паре `(version, arch)` — берётся из официального `sha256sums` релиза под конкретный tarball.

**Документированное послабление под зеркала (опциональный `sha256`).** Эталонный [`node-exporter/`](../../examples/destiny/node-exporter/) объявляет `sha256` как `optional` с `default: ""` (`pattern: "^(sha256:[0-9a-f]{64})?$"`): задан → `core.url.fetched` верифицирует до публикации; пуст → скачивание **без проверки целостности**, idempotency по SHA-256 содержимого. Это сознательный компромисс под зеркала / nexus-proxy, где хэш может быть недоступен заранее, а **не** ослабление правила выше. Опциональность хэша в самой destiny согласована с тем, что fail-closed-контракт держится **выше по стеку** — у вызывающего сценария (`monitoring` делает `node_exporter_sha256` обязательным). Условия применимости послабления:

- default-безопасный путь — **задать хэш**; пустой `sha256` повышает supply-chain-риск и явно помечен предупреждением в `input:`-описании самой destiny;
- послабление допустимо только в паре с `base_url`-override на доверенное внутреннее зеркало; для скачивания напрямую из публичного GitHub Releases держите хэш обязательным (как делает service-уровень);
- сценарий, претендующий на **строгий** prod-grade-контракт (без оговорок), объявляет хэш `required: true` без `default` у себя и пробрасывает его в destiny.

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
- [`examples/destiny/node-exporter/`](../../examples/destiny/node-exporter/) — эталон конвенции: бинарь `node_exporter`, stateful-аккаунт `node_exporter` (§2 stateful-ветка), version-aware install (`unless --version`, §6), systemd-hardening (§3), привилегированные textfile-коллекторы smartmon/nvme/ipmi под systemd-sandbox (§3b), опциональный `sha256` под зеркала (§7 послабление).

## Конвенция имён папок в `examples/`

Папка примера в `examples/destiny/` и `examples/service/` называется **голым именем зависимости без приставки родителя** (`destiny-`/`service-`): каталог — это `node-exporter/`, `redis/`, `monitoring/`, а не `destiny-node-exporter/` / `service-monitoring/`. Тип (destiny или service) определяется родительским каталогом, дублировать его в имени папки не нужно. Приставки соль-плагинов (`soul-mod-`/`soul-cloud-`/`soul-ssh-`) — **остаются**: это имена бинарей плагинов, а не папок-примеров.
