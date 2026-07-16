# Choir - endpoints of a named topology of hosts within an incarnation

Domain section [Operator API](../operator-api.md): endpoints `/v1/incarnations/{name}/choirs*` - CRUD topology Choir/Voice inside the incarnation (declared-"choir part", [ADR-044](../../adr/0044-choir.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). **There is no MCP side** - Choir does not have MCP tools installed ([mcp-tools/choirs.md](../mcp-tools/choirs.md)).

## Endpoint sections

Mapping endpoint ↔ permission (table of 6 routes) - in the root [operator-api.md → Choir (6)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`ChoirCreateRequest` / `Choir` / `ChoirListReply` / `VoiceAddRequest` / `Voice` / `VoiceListReply` - **source of truth in form**).

Choir belongs to the incarnation → all six routes carry the scope selector `incarnation`/`service`/`coven` (along path-`{name}`, the same `IncarnationScopeSelector` as for incarnation mutations); bare/`*` — unrestricted. Connect only when the ChoirDB pool is configured (otherwise `404`). Voice-actions hyphenated (`add-voice`/`remove-voice`) according to the grammar permission `<resource>.<action>`. `created_by_aid`/`added_by_aid` are taken from the JWT context, NOT from the body.

| Method/Path | Permission |
|---|---|
| `POST /v1/incarnations/{name}/choirs` | `choir.create` |
| `GET /v1/incarnations/{name}/choirs` | `choir.list` |
| `DELETE /v1/incarnations/{name}/choirs/{choir}` | `choir.delete` |
| `POST /v1/incarnations/{name}/choirs/{choir}/voices` | `choir.add-voice` |
| `GET /v1/incarnations/{name}/choirs/{choir}/voices` | `choir.list` |
| `DELETE /v1/incarnations/{name}/choirs/{choir}/voices/{sid}` | `choir.remove-voice` |

- **`POST …/choirs`** (`choir.create`): Creates a Choir. `choir_name` is validated by `^[a-z][a-z0-9_-]*$`; `min_size`/`max_size` - opt. sane-bounds(`> 0`, `min ≤ max`). Response `201 Choir`; `409` (name taken), `422` (broken format/bounds).
- **`GET …/choirs`** (`choir.list`): list of incarnation Choirs (sort `choir_name`). Non-existent incarnation → `200 + items=[]`. Response `200 ChoirListReply`.
- **`DELETE …/choirs/{choir}`** (`choir.delete`): Removes Choir with a cascade of its Voices (`ON DELETE CASCADE`). Response `204`; `404`.
- **`POST …/choirs/{choir}/voices`** (`choir.add-voice`): Adds Voice (SID Choir membership). **Membership invariant:** Voice is created only for a SID that is already a member of the incarnation (`souls.coven[]` contains `incarnation.name`); SIDs outside of membership → `422` (violating SIDs in `detail`). `role`/`position` - opt. declared attributes. Response `201 Voice`.
- **`GET …/choirs/{choir}/voices`** (`choir.list`): list of Choir Voices (sort `position` NULL-last, then `sid`). Non-existent Choir → `200 + items=[]`. Response `200 VoiceListReply`.
- **`DELETE …/choirs/{choir}/voices/{sid}`** (`choir.remove-voice`): removes Voice. Response `204`; `404`.

Mutating-CRUD audited(`choir.created` / `choir.deleted` / `choir.voice_added` / `choir.voice_removed`); payload is written by the handler itself - choir/voice-snapshot is available only after a successful mutation.
