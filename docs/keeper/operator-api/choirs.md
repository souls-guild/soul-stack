# Choir — endpoints именованной топологии хостов внутри инкарнации

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/incarnations/{name}/choirs*` — CRUD топологии Choir/Voice внутри инкарнации (declared-«партия хора», [ADR-044](../../adr/0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)). Conventions, error-format, pagination, mapping-таблица — в корневом [operator-api.md](../operator-api.md). **MCP-стороны нет** — у Choir MCP-tool-ов не заведено ([mcp-tools/choirs.md](../mcp-tools/choirs.md)).

## Endpoint-секции

Mapping endpoint ↔ permission (таблица 6 роутов) — в корневом [operator-api.md → Choir (6)](../operator-api.md#choir-6--именованная-топология-хостов-внутри-инкарнации-adr-044). Полная схема request/response — [`openapi.yaml`](../openapi.yaml) (`ChoirCreateRequest` / `Choir` / `ChoirListReply` / `VoiceAddRequest` / `Voice` / `VoiceListReply` — **источник правды по форме**).

Choir принадлежит инкарнации → все шесть роутов несут scope-селектор `incarnation`/`service`/`coven` (по path-`{name}`, тот же `IncarnationScopeSelector`, что у incarnation-мутаций); bare/`*` — unrestricted. Подключаются только при сконфигурированном ChoirDB-пуле (иначе `404`). Voice-actions hyphenated (`add-voice`/`remove-voice`) по грамматике permission `<resource>.<action>`. `created_by_aid`/`added_by_aid` берутся из JWT-контекста, НЕ из тела.

| Метод / Path | Permission |
|---|---|
| `POST /v1/incarnations/{name}/choirs` | `choir.create` |
| `GET /v1/incarnations/{name}/choirs` | `choir.list` |
| `DELETE /v1/incarnations/{name}/choirs/{choir}` | `choir.delete` |
| `POST /v1/incarnations/{name}/choirs/{choir}/voices` | `choir.add-voice` |
| `GET /v1/incarnations/{name}/choirs/{choir}/voices` | `choir.list` |
| `DELETE /v1/incarnations/{name}/choirs/{choir}/voices/{sid}` | `choir.remove-voice` |

- **`POST …/choirs`** (`choir.create`): создаёт Choir. `choir_name` валидируется `^[a-z][a-z0-9_-]*$`; `min_size`/`max_size` — опц. sane-bounds (`> 0`, `min ≤ max`). Response `201 Choir`; `409` (имя занято), `422` (битый формат/bounds).
- **`GET …/choirs`** (`choir.list`): список Choir-ов инкарнации (sort `choir_name`). Несуществующая incarnation → `200 + items=[]`. Response `200 ChoirListReply`.
- **`DELETE …/choirs/{choir}`** (`choir.delete`): удаляет Choir каскадом его Voice-ов (`ON DELETE CASCADE`). Response `204`; `404`.
- **`POST …/choirs/{choir}/voices`** (`choir.add-voice`): добавляет Voice (членство SID в Choir-е). **Инвариант членства:** Voice создаётся только для SID, который уже член инкарнации (`souls.coven[]` содержит `incarnation.name`); SID вне членства → `422` (нарушившие SID-ы в `detail`). `role`/`position` — опц. declared-атрибуты. Response `201 Voice`.
- **`GET …/choirs/{choir}/voices`** (`choir.list`): список Voice-ов Choir-а (sort `position` NULL-last, затем `sid`). Несуществующий Choir → `200 + items=[]`. Response `200 VoiceListReply`.
- **`DELETE …/choirs/{choir}/voices/{sid}`** (`choir.remove-voice`): убирает Voice. Response `204`; `404`.

Mutating-CRUD аудируется (`choir.created` / `choir.deleted` / `choir.voice_added` / `choir.voice_removed`); payload пишет сам handler — choir/voice-snapshot доступен только после успешной мутации.
