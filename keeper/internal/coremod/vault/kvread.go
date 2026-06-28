// Package vault реализует keeper-side core-модуль `core.vault`
// (ADR-017, docs/architecture.md → ADR-017) — единый модуль с диспетчеризацией
// по state (паттерн `core.cloud` / `core.choir`).
//
// Состояния:
//   - kv-read ([StateRead], applyReadKV): прочитать секрет из Vault KV (v1/v2)
//     на keeper-стороне и отдать в register-output задачи.
//   - kv-present ([StatePresent], applyPresent в kvpresent.go): generate-if-absent
//     — гарантировать существование секретов, сгенерировав недостающие
//     криптослучайным значением по описанной автором password-policy.
//
// Оба состояния обслуживает один [Module] (ключ Registry — base-имя `core.vault`,
// state приходит в pluginv1.ApplyRequest.state и маршрутизируется в Validate/Apply).
//
// Зачем kv-read существует, если в CEL есть implicit `${ vault(...) }`:
// implicit-vault — дёшев для рендера, но в audit-trail не отдельной записью.
// Этот state — explicit-форма для случаев, требующих явного audit-event-а
// `vault.kv-read` (PCI-DSS, SOC2, compliance-аккуратный код). kv-present
// аналогично пишет `vault.kv-present` при генерации.
//
// Output kv-read:
//
//	register.<name>.data       — map[string]any с извлечёнными ключами.
//	register.<name>.path       — эхо запрошенного пути.
//	register.<name>.fields     — список ключей в data (sorted).
//
// Output kv-present:
//
//	register.<name>.generated  — map path → [сгенерированные поля] (без значений).
//
// Сами значения секретов в audit-payload и output обоих состояний **не**
// кладутся: фиксируется только факт (path + имена полей). Output kv-read
// маскируется на write-path-е destiny/scenario (shared/audit.MaskSecrets) —
// известные секретные ключи; output kv-present имён значений не несёт вовсе.
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
// адреса задачи — base + state (`core.vault.kv-read` / `core.vault.kv-present`);
// state приходит в pluginv1.ApplyRequest.state.
const Name = "core.vault"

// StateRead — состояние чтения. Совпадает с author-state-суффиксом `kv-read`
// адреса `core.vault.kv-read` (SplitModuleAddr выделит "kv-read").
const StateRead = "kv-read"

// VaultWriter — узкое подмножество keeper/internal/vault.Client, нужное модулю:
// чтение (kv-read + проверка присутствия в kv-present) и запись (генерация в
// kv-present). Сужение поверх *vault.Client упрощает unit-тесты (fake без HTTP).
//
// kv-read использует только ReadKV (write-путь не вызывается на read-state); один
// общий интерфейс держит wire-up единым (`Deps.Vault` — *vault.Client, умеет оба).
type VaultWriter interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// AuditWriter — узкая зависимость для audit-event-ов `vault.kv-read` /
// `vault.kv-present`. Имя совпадает с shared/audit.Writer-сигнатурой; типизация
// локально, чтобы модуль не таскал транзитивно весь pipeline.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module — реализация sdk/module.SoulModule поверх VaultWriter+AuditWriter.
// Один модуль на base-имя `core.vault`; state (`kv-read`/`kv-present`)
// маршрутизируется внутри Validate/Apply.
type Module struct {
	Vault VaultWriter
	Audit AuditWriter
}

// New — wire-helper.
func New(v VaultWriter, a AuditWriter) *Module {
	return &Module{Vault: v, Audit: a}
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateRead:
		if _, err := util.StringParam(req.Params, "path"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.OptStringSliceParam(req.Params, "fields"); err != nil {
			errs = append(errs, err.Error())
		}
	case StatePresent:
		errs = append(errs, validatePresent(req)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want %s/%s)", req.State, StateRead, StatePresent))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply маршрутизирует по state: kv-read → applyReadKV (read-only, changed=false),
// kv-present → applyPresent (generate-if-absent, kvpresent.go). Неизвестный state
// → failed-event (scenario-applier зайдёт в onfail).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case StateRead:
		return m.applyReadKV(req, stream)
	case StatePresent:
		return m.applyPresent(req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// applyReadKV делает Vault.ReadKV → фильтр по полю `fields` → пишет audit-event.
// changed: всегда false — read-операция, не мутирует state.
func (m *Module) applyReadKV(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	wantedFields, err := util.OptStringSliceParam(req.Params, "fields")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// ReadKV отдаёт развёрнутый плоский payload (поля секрета) для ОБЕИХ
	// версий KV — версия резолвится прозрачно в vault.Client (ADR-017(b),
	// amendment 2026-06-22). Здесь обёртки `{data,metadata}` уже нет.
	payload, err := m.Vault.ReadKV(ctx, path)
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
