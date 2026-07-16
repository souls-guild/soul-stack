# smoke-nginx

Минимальный service для L3a E2E pilot ([ADR-039](../../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).

## Что демонстрирует

- Service с непустым `state_schema` и двумя scenario-полями (`nginx_package`,
  `nginx_service`), которые фиксируются в `incarnation.state` после успешного
  apply (через `state_changes.sets`).
- Scenario `create` из двух последовательных core-задач:
  - `core.pkg.installed name=nginx` — установка пакета;
  - `core.service.running name=nginx enabled=true` — запуск и enable systemd-юнита.
- Минимальный `input:` (только `hostname` — обязательный) — пример обязательного
  поля без shape-проверки на конкретном значении (любой непустой string).

## Где используется

- `tests/e2e/smoke_nginx_test.go` — Go-test L3a pilot, прогоняет scenario
  `create` через harness, soul-stub отвечает scripted `RunResult: success`,
  тест валидирует apply_runs / incarnation.state / audit / metrics.

## Чего здесь нет

- Реального шаблона `nginx.conf` — pilot тестирует контракт scenario-runner ↔
  apply_runs ↔ audit, не реальное apply. L3b (real soul-binary в контейнере)
  получит свой fixture с реальным render-ом конфига.
- Идемпотентного destroy-scenario — pilot фокусируется на happy-path create.

## Trial-проверки

L0/L1 (`soul-trial`) для этого example пока не добавлены — pilot целит в L3a.
Trial-фикстура (`_trial/`) появится отдельным slice-ом, если потребуется
purely-hermetic-render assertion для smoke-nginx.
