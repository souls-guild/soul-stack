# Choir — MCP-tools именованной топологии хостов

Доменная секция [каталога MCP-tools](../mcp-tools.md): домен Choir (`/v1/incarnations/{name}/choirs*`, топология Choir/Voice внутри инкарнации, [ADR-044](../../adr/0044-choir.md#adr-044-choir--именованная-топология-хостов-внутри-инкарнации)). Источник правды по семантике — [operator-api/choirs.md](../operator-api/choirs.md).

### Choir (0)

У Choir **MCP-tool-ов нет** — домен REST-only. В каталоге `keeper/internal/mcp/manifest.go` нет ни одного `keeper.choir.*`-tool-а: топология хостов внутри инкарнации (Choir/Voice) управляется через Operator API (`POST/GET/DELETE /v1/incarnations/{name}/choirs*` + voices). MCP-симметрия для домена Choir не реализована и в MVP-каталог 72 tool-ов не входит; появится отдельным PR при необходимости (расширение каталога — only-add, [mcp-tools.md → Будущие чтения и удаления](../mcp-tools.md#будущие-чтения-и-удаления)).

> **Ремарка о drift в OpenAPI-описаниях.** Текстовые `description` choir-операций в [`openapi.yaml`](../openapi.yaml) упоминают `MCP-tool: keeper.choir.create` / `keeper.choir.delete` / `keeper.choir.add-voice` / `keeper.choir.remove-voice` — этих tool-ов в `manifest.go` фактически нет (домен REST-only). Источник правды по наличию tool-а — `manifest.go`; описания в спеке устарели и подлежат правке отдельной задачей (не входит в эту mapping-инвентаризацию).
