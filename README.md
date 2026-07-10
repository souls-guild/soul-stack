# Soul Stack

Система управления конфигурациями в духе SaltStack, но со своим словарём имён в «душевной» метафоре. Центральный сервер (**Keeper**) держит долгоживущие защищённые соединения с агентами на хостах (**Souls**) и приводит каждый хост к описанному желаемому состоянию (**Destiny**).

> Статус: закрытая малая бета. Подходит для единиц операторов и флота до сотен хостов. Что в бету **не** входит — [docs/known-limitations.md](docs/known-limitations.md).

## Словарь

Если знакомы с SaltStack — соответствие терминов:

| Soul Stack | SaltStack | Смысл |
|---|---|---|
| **Keeper** | master | Хранитель, центральный узел. |
| **Souls** | minions | Управляемые агенты на хостах. |
| **Destiny** | states | Желаемое состояние хоста после прогона. |
| **Soulprint** | grains | Факты о системе хоста (ОС, ядро, CPU, сеть). |
| **Essence** | pillars | Параметры/значения, подставляемые в Destiny. |

Полный словарь имён — [docs/naming-rules.md](docs/naming-rules.md).

## Ключевые свойства

- **HA из коробки.** Keeper — горизонтально масштабируемый stateless-кластер поверх общей Postgres и Redis. Любой инстанс обслуживает любой запрос; падение одного не роняет управление флотом.
- **mTLS-идентичность флота.** Связь Keeper↔Soul — gRPC bidirectional stream поверх mTLS. Каждый Soul онбордится через CSR (приватный ключ никогда не покидает хост), получает короткоживущий сертификат (**SoulSeed**), который автоматически ротируется.
- **Встроенный RBAC.** Доступ операторов (**Archon**) — через JWT и permission-строки с scope по coven / service / incarnation. По умолчанию — deny.
- **Vault как единый secret-store.** Все секреты (DSN Postgres, JWT signing-key, PKI для SoulSeed) живут в Vault; на диск Keeper-кластера секреты не материализуются.
- **OpenAPI + MCP.** Первичный интерфейс оператора — REST (OpenAPI) и MCP-сервер для AI-агентов. CLI — тонкая обёртка, не основной путь.
- **Observability из коробки.** Prometheus-метрики и OpenTelemetry-трейсы во всех бинарях, встроенная ротация логов, hot-reload конфигурации.

## Архитектура одним абзацем

Три бинаря (ADR-004): `keeper` (центральный сервер — gRPC, OpenAPI, MCP, фоновый Reaper), `soul` (демон-агент на управляемом хосте) и `soul-lint` (офлайн-линтер артефактов). Стрим всегда инициирует Soul — на управляемых хостах нет открытых входящих портов. Холодное состояние кластера — в Postgres, горячий слой (presence, lease, лидер-выборы) — в Redis. Обязательный инфра-контур — **Postgres + Redis + Vault** ([ADR-053](docs/adr/0053-dependency-tiers.md)).

## С чего начать

- **[docs/getting-started.md](docs/getting-started.md)** — поднять single-keeper + инфру, забутстрапить первого Архонта, онбордить один Soul и применить простой сценарий. ~30 минут.
- **[docs/known-limitations.md](docs/known-limitations.md)** — что не входит в бету.
- **[docs/operations/](docs/operations/README.md)** — операционный runbook для прод-инсталляции (deployment / HA / backup / disaster recovery).
- **[docs/README.md](docs/README.md)** — индекс всей документации.
- **[docs/architecture.md](docs/architecture.md)** — обзор архитектуры и ссылки на ADR (источник правды по дизайну).

## Безопасность и обратная связь

Закрытая бета, поддержка best-effort (без SLA).

- **Баги и неожиданное поведение** — [GitHub Issues](https://github.com/co-cy/soul-stack/issues) (шаблон «Bug report»). Версию берите из `keeper version`.
- **Уязвимости безопасности** — приватный Security Advisory, **не** публичный issue: [SECURITY.md](SECURITY.md).
- **Куда обращаться и уровень поддержки** — [SUPPORT.md](SUPPORT.md).

## Лицензия

Ядро (этот репозиторий) — [Business Source License 1.1](LICENSE) (fair-code): исходники открыты, production-использование разрешено, **кроме** перепродажи Soul Stack третьим лицам как hosted/managed-сервиса; каждая версия автоматически становится [Apache 2.0](https://www.apache.org/licenses/LICENSE-2.0) через 2 года (Change Date). SDK, примеры и плагины — Apache 2.0; enterprise-фичи — отдельная коммерческая лицензия ([ADR-016](docs/adr/0016-parity-license.md)). Что вам можно простыми словами — [LICENSING.md](LICENSING.md).
