# Cadence — MCP-tools регулярных запусков

Доменная секция [каталога MCP-tools](../mcp-tools.md): домен Cadence (`/v1/cadences*`, регулярные запуски Voyage по расписанию, [ADR-046](../../adr/0046-cadence.md#adr-046-cadence--регулярные-запуски-scheduledrecurring-voyage)). Источник правды по семантике — [operator-api/cadences.md](../operator-api/cadences.md).

### Cadence (0)

У Cadence **MCP-tool-ов нет** — домен REST-only. В каталоге `keeper/internal/mcp/manifest.go` нет ни одного `keeper.cadence.*`-tool-а: управление расписаниями ведётся через Operator API (`POST/GET/PATCH/DELETE /v1/cadences*` + `enable`/`disable` + `runs`). Если оператору-LLM нужен периодический прогон — он создаёт Voyage напрямую ([mcp-tools/voyages.md](voyages.md)) либо обращается к человеку-оператору за заведением Cadence через UI/REST.

MCP-симметрия для домена Cadence не реализована и в MVP-каталог 72 tool-ов не входит; появится отдельным PR при необходимости (расширение каталога — only-add, [mcp-tools.md → Будущие чтения и удаления](../mcp-tools.md#будущие-чтения-и-удаления)).
