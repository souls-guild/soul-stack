# core.archive

Распаковка архивов в каталог. **Soul-side**, статически встроен в `soul`-бинарь.
Реализация — [`soul/internal/coremod/archive/archive.go`](../../../../soul/internal/coremod/archive/archive.go).

Поддерживаемые форматы: **tar** / **tar.gz** (`.tgz`) / **tar.bz2** (`.tbz2`) /
**zip**. `format` опционален — auto-detect по расширению `path`. Распаковка идёт
**in-process** средствами Go stdlib (`archive/tar`, `archive/zip`,
`compress/gzip`, `compress/bzip2`) — без внешних утилит (`tar` / `unzip`) и без
порождения подпроцессов. Это снимает зависимость от хостовых бинарей и даёт
per-entry контроль безопасности (zip-slip / zip-bomb / symlink-политика),
недоступный backend-утилитам. tar.bz2 — только распаковка (bzip2 в stdlib
decompress-only; для MVP этого достаточно).

## States

| State | Назначение | Идемпотентность (когда `changed=true`) |
|---|---|---|
| `extracted` | Архив `path` распакован в каталог `dest`. | После распаковки SHA-256 исходного архива пишется в `<dest>/.soul-archive.sha256`. `changed=true`, если маркера нет либо его хэш ≠ хэшу текущего архива. Совпал — `changed=false` (no-op). Это grounded-проверка «архив тот же», а не «все файлы внутри `dest` на месте». |

## extracted — params

| Param | Тип | Required / default | Смысл |
|---|---|---|---|
| `path` | string | required | Путь к архиву-источнику. |
| `dest` | string | required | Каталог назначения. Создаётся (`MkdirAll`, mode `0755`), если не существует. |
| `format` | string | optional (default — auto-detect) | Принудительный формат: `tar` / `tar.gz` (`tgz`) / `tar.bz2` (`tbz2`) / `zip`. Пусто/не задан → формат определяется по суффиксу `path`; если суффикс не распознан — шаг падает (`cannot auto-detect format`). |
| `max_size` | string | optional (default `1GiB`) | Потолок **суммарного распакованного** размера (zip-bomb-защита). Голое число = байты, либо число с бинарным суффиксом `KiB` / `MiB` / `GiB` (регистр суффикса не важен). Десятичные SI-суффиксы (`KB` / `MB` / `GB`) и дроби **не поддерживаются** — нераспознанный суффикс или мусор → явная ошибка конфигурации (`invalid size`), а не тихий отброс хвоста. Значение `≤ 0` тоже отвергается. Превышение → шаг падает (`exceeded max_size`). Ratio-лимита нет — только абсолютный потолок. |
| `max_entries` | integer | optional (default `100000`) | Потолок числа записей в архиве (zip-bomb-защита). Превышение → шаг падает (`exceeded max_entries`). |

## Capabilities / side-effects

- **Меняет файловую систему:** создаёт каталог `dest`, распаковывает в него
  содержимое архива, пишет маркер `.soul-archive.sha256` в `dest`. Для системных
  путей требует соответствующих прав (на практике — root, см.
  [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- **Не порождает подпроцессов.** Распаковка — in-process на Go stdlib; манифест
  объявляет только [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum),
  `exec_subprocess` **снят** (внешние `tar`/`unzip` больше не вызываются).
  Хостовые утилиты распаковки не нужны.
- Маркер-файл лежит **внутри** `dest` — учитывайте при последующей сверке
  содержимого каталога другими шагами.

## Output / register

`extracted` отдаёт `{ path, dest, sha256, extracted: true }`, где `sha256` —
хэш исходного архива (он же содержимое маркера). На no-op (хэш совпал) набор
полей тот же, отличается только `changed=false`.

## Пример

```yaml
# Распаковать скачанный tarball в каталог. format auto-detect по .tar.gz.
# Идемпотентно: core.archive пишет marker и повторно не распаковывает тот же архив.
- name: Extract node_exporter tarball
  module: core.archive.extracted
  params:
    path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
    dest: "${ '/tmp/node_exporter-' + input.version }"
```

(из [`examples/destiny/node-exporter/tasks/install.yml`](../../../../examples/destiny/node-exporter/tasks/install.yml))

## Безопасность

Распаковка in-process даёт per-entry контроль над каждой записью архива до её
материализации. Инварианты ниже жёсткие — поведением backend-утилиты не
определяются и (кроме лимитов zip-bomb) флагами не отключаются.

- **zip-slip / path-traversal — fail-fast.** Для каждой записи целевой путь
  строится через [`filepath-securejoin`](https://github.com/cyphar/filepath-securejoin)
  относительно `dest`, плюс лексический детект escape. Запись с `..` либо
  абсолютным путём, выводящая за пределы `dest`, → шаг **падает целиком**
  (`archive: entry %q escapes dest`), уже распакованные файлы остаются, маркер
  `.soul-archive.sha256` **не** пишется (повтор не считается успешным). Это
  fail-fast, а **не** тихий clamp: запись наружу не создаётся ни в `dest`, ни вне
  него.
- **zip-bomb — абсолютные лимиты.** Суммарный распакованный размер ограничен
  `max_size` (дефолт `1GiB`), число записей — `max_entries` (дефолт `100000`).
  Размер считается через `io.LimitReader` на каждую запись + аккумулятор;
  превышение любого лимита → `failed` с указанием, какой лимит пробит
  (`exceeded max_size` / `exceeded max_entries`). Ratio-лимита (степень сжатия)
  нет — только абсолютные потолки. Оба настраиваются параметрами.
- **symlink — within-dest only.** Symlink из архива создаётся **только** если его
  target (резолвнутый относительно директории самого symlink-а) остаётся внутри
  `dest`. Абсолютный target или относительный, выводящий за `dest`, →
  `archive: symlink %q target escapes dest`, шаг падает. Это закрывает symlink-
  vector обхода zip-slip (symlink наружу + последующая запись «сквозь» него).
- **Запись через symlink-каталог, созданный внутри того же архива, не
  поддерживается.** Если архив содержит symlink-каталог *внутри* `dest`
  (within-dest, легитимный сам по себе), а затем запись по пути «сквозь» этот
  symlink (`alias/file.txt`, где `alias` → существующий каталог) — шаг падает с
  `escapes dest`. Это **fail-closed**: `securejoin` резолвит symlink по уже
  лежащему на диске пути, резолвнутый результат ≠ наивного `filepath.Join`, и
  модуль отвергает запись. Проверка идёт по **резолвнутому** пути, а не лексически
  — наивная замена `securejoin` лексическим `Join` сломала бы эту защиту. Цена —
  легитимный «запиши через свой же symlink-каталог» в одном архиве не работает;
  это сознательный безопасный отказ, target-каталог нужно адресовать напрямую.
- **setuid / setgid / sticky маскируются всегда.** mode записи применяется как
  `entry.Mode() & 0o777` — биты `setuid`/`setgid`/`sticky` снимаются безусловно
  (anti-privesc: архив не может протащить setuid-root-бинарь). Это жёсткий
  инвариант, не флаг. owner/group из архива **не** берутся — файлы получают
  владельца процесса `soul`-агента.
- **Неподдерживаемые типы записей — явная ошибка.** hardlink (`TypeLink`),
  устройства (`TypeBlock`/`TypeChar`), fifo (`TypeFifo`), socket → шаг падает
  (`archive: entry %q: unsupported type`), а не молча пропускается. В MVP эти типы
  не материализуются. Служебные PAX/GNU-заголовки и sparse-метаданные молча
  игнорируются — они не добавляют файлов в дерево.
- **Checksum — для идемпотентности, не для верификации источника.** SHA-256 в
  `register.<name>.sha256` и маркере считается по **уже** лежащему на диске
  файле-источнике (`hashFile`) — это «тот же ли архив, что в прошлый раз», а не
  «совпал ли он с ожидаемым доверенным хэшем». Верификации против эталонного
  чексумма/подписи модуль не делает; если она нужна — сверяйте хэш отдельным
  шагом (например `register` + `failed_when:`) до распаковки.
- **Привилегии.** Манифест
  ([`archive.yaml`](../../../../shared/coremanifest/archive.yaml)) объявляет
  только [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum)
  (запись за пределы `/var/lib/soul-stack/`), но **не** `run_as_root` и больше
  **не** `exec_subprocess` (подпроцессы не порождаются). Модуль выполняется с
  привилегиями процесса `soul`-агента; для распаковки в системные пути агент на
  практике работает под root — поэтому setuid-маскинг и within-dest-инварианты
  тем ценнее.
- **Доверие к источнику всё ещё уместно.** Инварианты выше закрывают
  zip-slip / zip-bomb / privesc-через-setuid, но модуль не верифицирует **подпись
  / происхождение** архива. Для недоверенных артефактов (upload, сторонний build)
  по-прежнему предпочтителен изолированный непривилегированный каталог `dest` и
  отдельная проверка подписи/хэша до распаковки:

  ```yaml
  # Доверенный tarball, известный из самого Destiny, в свой каталог.
  - name: Extract node_exporter tarball
    module: core.archive.extracted
    params:
      path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
      dest: "${ '/tmp/node_exporter-' + input.version }"
      max_size: "500MiB"
  ```

## См. также

- [README.md](../../README.md) — каталог core-модулей.
- [core/url/README.md](../url/README.md) — загрузка архива по URL перед распаковкой.
- [core/cmd/README.md](../cmd/README.md) — раскладка распакованного бинаря (`install`).
- [soul/modules.md](../../../soul/modules.md) — хостовая сторона модулей и кеш.
- [naming-rules.md → Модули Destiny](../../../naming-rules.md#модули-destiny) — словарь имён.
- [ADR-015](../../../adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список) — список core MVP.
