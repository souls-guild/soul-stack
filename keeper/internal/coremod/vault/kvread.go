// Package vault реализует keeper-side core-модуль `core.vault.kv-read`
// (ADR-017, docs/architecture.md → ADR-017).
//
// Состояние:
//   - kv-read: прочитать секрет из Vault KV v2 на keeper-стороне и
//     отдать в register-output задачи.
//
// Зачем существует, если в CEL есть implicit `${ vault(...) }`:
// implicit-vault — дёшев для рендера, но в audit-trail не отдельной записью.
// Этот модуль — explicit-форма для случаев, требующих явного audit-event-а
// `vault.kv-read` (PCI-DSS, SOC2, compliance-аккуратный код).
//
// Output:
//
//	register.<name>.data       — map[string]any с извлечёнными ключами.
//	register.<name>.path       — эхо запрошенного пути.
//	register.<name>.fields     — список ключей в data (sorted).
//
// Сами значения секретов в audit-payload **не** кладутся: фиксируется только
// факт чтения (path + fields). Output задачи маскируется на write-path-е
// destiny/scenario (shared/audit.MaskSecrets) — известные секретные ключи.
package vault

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.vault.kv-read` (base + state); state `kv-read`
// приходит в pluginv1.ApplyRequest.state.
const Name = "core.vault"

// StateRead — единственное состояние модуля. Совпадает с author-state-суффиксом
// `kv-read` адреса `core.vault.kv-read` (SplitModuleAddr выделит "kv-read").
const StateRead = "kv-read"

// VaultReader — узкое подмножество keeper/internal/vault.Client, нужное модулю.
// Сужение поверх *vault.Client упрощает unit-тесты (fake без поднятия HTTP).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// AuditWriter — узкая зависимость для audit-event-а `vault.kv-read`.
// Имя совпадает с shared/audit.Writer-сигнатурой; типизация локально, чтобы
// модуль не таскал транзитивно весь pipeline.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module — реализация sdk/module.SoulModule поверх VaultReader+AuditWriter.
type Module struct {
	Vault VaultReader
	Audit AuditWriter
}

// New — wire-helper.
func New(v VaultReader, a AuditWriter) *Module {
	return &Module{Vault: v, Audit: a}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != StateRead {
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s)", req.State, StateRead))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringSliceParam(req.Params, "fields"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply делает Vault.ReadKV → фильтр по полю `fields` → пишет audit-event.
// changed: всегда false — read-операция, не мутирует state.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != StateRead {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}

	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	wantedFields, err := util.OptStringSliceParam(req.Params, "fields")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	raw, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("vault read %q: %v", path, err))
	}

	// KV v2 формат: data.data → user-payload. ReadKV возвращает Secret.Data,
	// который у KV v2 = {"data": {...}, "metadata": {...}}. Достаём data.
	payload, err := extractKVData(raw)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("vault read %q: %v", path, err))
	}

	out := filterFields(payload, wantedFields)
	fields := sortedKeys(out)

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventVaultKVRead,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"path":   path,
				"fields": toAnySlice(fields),
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			// audit-фейл не блокирует чтение, но логируется как failed —
			// иначе модуль молча пропустит обязательный compliance-шаг.
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	resp := map[string]any{
		"path":   path,
		"data":   out,
		"fields": toAnySlice(fields),
	}
	return util.SendFinal(stream, false, resp)
}

// extractKVData аккуратно достаёт `data` из KV v2 secret-response. Vault
// клиент возвращает map[string]any с верхним ключом `data` (вложенный
// payload) + `metadata`. Если raw — не KV v2 формат (legacy KV v1, кто-то
// разрешит mount), отдаём raw как есть.
func extractKVData(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	if inner, ok := raw["data"]; ok {
		if m, ok := inner.(map[string]any); ok {
			return m, nil
		}
		return nil, fmt.Errorf("kv v2 data: expected object, got %T", inner)
	}
	// Fallback — KV v1 (без обёртки data/metadata).
	return raw, nil
}

// filterFields оставляет только указанные ключи. Пустой/nil wanted → весь payload.
// Запрошенный, но отсутствующий ключ — silently пропускается (это не failure-кейс
// модуля: caller хотел опциональные поля; на чтение secret уже потрачен audit-event).
func filterFields(payload map[string]any, wanted []string) map[string]any {
	if len(wanted) == 0 {
		return cloneMap(payload)
	}
	out := make(map[string]any, len(wanted))
	for _, k := range wanted {
		if v, ok := payload[k]; ok {
			out[k] = v
		}
	}
	return out
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toAnySlice(xs []string) []any {
	if xs == nil {
		return []any{}
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
