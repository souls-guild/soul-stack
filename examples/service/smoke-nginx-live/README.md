# smoke-nginx-live — L3b flagship-smoke

L3b real-soul-in-container smoke ([ADR-039](../../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря)).

Параллель с [smoke-nginx](../smoke-nginx/README.md) (L3a): тот же end-to-end
сценарий «install nginx → render config → start service», но идущий через
РЕАЛЬНЫЙ apt-install в Debian-12-soul-container, а не через scripted
soul-stub.

## Что демонстрирует

- `core.pkg.installed name=nginx` — реальная установка пакета (`apt-get
  install nginx`) внутри контейнера.
- `core.file.rendered` — рендер `/etc/nginx/sites-available/default` из
  шаблона `templates/nginx-default.conf.tmpl` с переменными из
  `input.hostname` и `essence.nginx_listen_port`.
- `core.service.running name=nginx enabled=true` — поднятие systemd-юнита.
- `core.service.restarted onchanges: [nginx_default_conf]` — реактивный
  рестарт только при изменении конфига (идемпотентность повторного apply).

Все четыре шага инлайн в scenario (без выделенной destiny): smoke не
переиспользуется в других сценариях, выделять destiny под него — оверхед
(см. [docs/scenario/concept.md → граница — рекомендация](../../../docs/scenario/concept.md)).

## Где используется

- `tests/e2e-live/smoke_nginx_live_test.go` — Go-test L3b flagship,
  прогоняет scenario `create` через harness, валидирует:
  - `apply_runs.status = success` (все строки);
  - `incarnation.state` содержит `nginx_package=installed`,
    `nginx_service=active`, `nginx_config_managed=true`;
  - audit-event `incarnation.scenario_started` с матчем incarnation/apply_id;
  - метрика `keeper_apply_runs_total >= 1`.

Container-side ассерты (`AssertHostPkgInstalled`, `AssertHostServiceActive`,
`AssertHostFileExists`) появляются в L3b-4 — здесь только Keeper-side
наблюдаемые свойства.

## Чего здесь нет

- Идемпотентного destroy-сценария — flagship фокусируется на happy-path
  create (повторный create на том же контейнере — идемпотентен, см.
  комментарии в scenario/create/main.yml, но отдельным тестом не покрыт).
- Multi-host прогона — добавится в L3b-5.
- HTTPS/upstream — минимальный server-block без TLS, для smoke достаточно.

## Trial-проверки

L0/L1 (`soul-trial`) для этого example пока не добавлены — flagship целит
в L3b. Trial-фикстура (`_trial/`) появится отдельным slice-ом, если
потребуется hermetic-render assertion (например для рендера nginx-конфига
без поднятия контейнера).

## Ожидаемое время прогона

~3–5 минут на нагруженной CI-машине: `apt-get update` (~30 c) + установка
nginx (~30 c) + старт systemd-юнита (~5 c) + поллинг apply_runs до success.
Тест ставит timeout 300 c (`WaitApplySuccess(t, applyID, 300)`).
