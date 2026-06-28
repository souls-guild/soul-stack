package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// StatePresent — состояние generate-if-absent. Совпадает с author-state-суффиксом
// `kv-present` адреса `core.vault.kv-present` (SplitModuleAddr выделит "kv-present").
//
// Парный близнец [StateRead]: kv-read читает секрет в register-output, kv-present
// гарантирует существование секрета (генерит недостающее криптослучайным
// значением по описанной автором password-policy), ничего из значений наружу
// не отдавая.
const StatePresent = "kv-present"

// defaultPasswordField — имя поля по умолчанию для target без явного `field`.
// Совпадает с конвенцией redis-секретов (`secret/redis/<inc>/users/<name>#password`).
const defaultPasswordField = "password"

// presentTarget — одна цель генерации: Vault-путь + имя поля + policy генерации.
// policy резолвится на этапе parse (step-level default + per-target override),
// поэтому Apply уже оперирует готовым алфавитом/длиной без повторного разбора.
type presentTarget struct {
	path   string
	field  string
	policy passwordPolicy
}

// validatePresent — runtime-страховка params kv-present (soul-lint валидирует
// author-форму статически). Проверяет targets и обе policy-формы.
func validatePresent(req *pluginv1.ValidateRequest) []string {
	var errs []string
	if _, err := parseTargets(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

// applyPresent: для каждого target читает путь; если поле отсутствует — генерит
// значение по его policy и пишет (read-merge-write, чтобы не затереть соседние
// поля того же пути); если присутствует — no-op. changed=true только когда
// реально что-то сгенерировано (как `core.soul.registered`).
//
// БЕЗОПАСНОСТЬ (ADR-010): сгенерированное ЗНАЧЕНИЕ не уходит в register-output,
// audit-payload, логи и OTel. Наружу — только факт + path + список
// сгенерированных полей (эталон sigil.KeyService.Introduce).
func (m *Module) applyPresent(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	targets, err := parseTargets(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// pendingWrites агрегирует генерацию ПО ПУТЯМ: несколько target-ов на один
	// путь (разные поля) сливаются в один WriteKV поверх существующих полей. Это
	// сохраняет соседние поля пути и не плодит лишних KV-версий.
	pendingWrites := make(map[string]map[string]any)
	// existing кэширует прочитанный payload каждого пути в пределах Apply, чтобы
	// два target-а на один путь не били ReadKV дважды и видели согласованную базу.
	existing := make(map[string]map[string]any)
	// generated — пути → отсортированный список сгенерированных полей (для output
	// и audit). ТОЛЬКО имена полей, без значений.
	generated := make(map[string][]string)

	for _, t := range targets {
		payload, ok := existing[t.path]
		if !ok {
			payload, err = m.readPath(ctx, t.path)
			if err != nil {
				return util.SendFailed(stream, fmt.Sprintf("vault read %q: %v", t.path, err))
			}
			existing[t.path] = payload
		}

		if fieldPresent(payload, t.field) {
			continue // секрет уже есть — не перезатираем
		}
		// Учитываем уже запланированную в этом же Apply генерацию того же поля
		// (два target-а с одним path+field): второй раз не генерим.
		if w, ok := pendingWrites[t.path]; ok {
			if _, planned := w[t.field]; planned {
				continue
			}
		}

		value, gerr := t.policy.generate()
		if gerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("generate secret for %q: %v", t.path, gerr))
		}
		w := pendingWrites[t.path]
		if w == nil {
			// База для merge — копия существующих полей пути (если он уже был);
			// сверху кладём сгенерированные. Так WriteKV (новая KV-версия) не
			// теряет соседние поля.
			w = cloneMap(payload)
			pendingWrites[t.path] = w
		}
		w[t.field] = value
		generated[t.path] = append(generated[t.path], t.field)
	}

	for path, data := range pendingWrites {
		if werr := m.Vault.WriteKV(ctx, path, data); werr != nil {
			// WriteKV не кладёт значения в текст ошибки (vault.Client-инвариант);
			// здесь тоже — только path.
			return util.SendFailed(stream, fmt.Sprintf("vault write %q: %v", path, werr))
		}
	}

	changed := len(generated) > 0
	genFields := generatedFieldsOutput(generated)

	if m.Audit != nil && changed {
		ev := &audit.Event{
			EventType: audit.EventVaultKVPresent,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"paths": genFields, // path → [сгенерированные поля]; значений нет
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	resp := map[string]any{
		"generated": genFields,
	}
	return util.SendFinal(stream, changed, resp)
}

// readPath читает payload пути; несуществующий путь — НЕ ошибка, а пустой
// payload (все его поля будут сгенерированы). Транспортные/политики-ошибки
// прокидываются наверх.
func (m *Module) readPath(ctx context.Context, path string) (map[string]any, error) {
	payload, err := m.Vault.ReadKV(ctx, path)
	if err != nil {
		if errors.Is(err, keepervault.ErrVaultKVNotFound) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return payload, nil
}

// parseTargets разбирает `params.targets` — обязательный непустой список объектов
// `{path: <str>, field?: <str>, policy?: <object>}`. Пустой/отсутствующий список
// — ошибка (модулю нечего гарантировать). `field` по умолчанию —
// [defaultPasswordField]. policy каждого target резолвится поверх step-level
// `params.policy` (общий дефолт шага) → [defaultPolicy] (если ни там, ни там нет).
func parseTargets(params *structpb.Struct) ([]presentTarget, error) {
	stepPolicy, err := parseStepPolicy(params)
	if err != nil {
		return nil, err
	}
	raw, err := util.ListParam(params, "targets")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("param \"targets\": empty list")
	}
	out := make([]presentTarget, 0, len(raw))
	for i, item := range raw {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param \"targets\"[%d]: expected object, got %T", i, item.Kind)
		}
		t, terr := parseTarget(sv.StructValue, i, stepPolicy)
		if terr != nil {
			return nil, terr
		}
		out = append(out, t)
	}
	return out, nil
}

// parseStepPolicy разбирает опциональный step-level `params.policy` — общий
// дефолт генерации для всех target-ов без своего `policy`. Отсутствует →
// [defaultPolicy].
func parseStepPolicy(params *structpb.Struct) (passwordPolicy, error) {
	obj, err := util.OptStructParam(params, "policy")
	if err != nil {
		return passwordPolicy{}, err
	}
	if obj == nil {
		return defaultPolicy(), nil
	}
	return parsePolicy(obj, defaultPolicy())
}

func parseTarget(s *structpb.Struct, idx int, stepPolicy passwordPolicy) (presentTarget, error) {
	path, err := util.StringParam(s, "path")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	if path == "" {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].path: empty", idx)
	}
	field, err := util.OptStringParam(s, "field")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	if field == "" {
		field = defaultPasswordField
	}
	// per-target policy override поверх step-level default.
	override, err := util.OptStructParam(s, "policy")
	if err != nil {
		return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
	}
	policy := stepPolicy
	if override != nil {
		policy, err = parsePolicy(override, stepPolicy)
		if err != nil {
			return presentTarget{}, fmt.Errorf("param \"targets\"[%d].%v", idx, err)
		}
	}
	return presentTarget{path: path, field: field, policy: policy}, nil
}

// fieldPresent — поле существует в payload и его значение непустое. Пустая
// строка трактуется как «нет» (повод сгенерировать): пустой пароль бесполезен.
func fieldPresent(payload map[string]any, field string) bool {
	v, ok := payload[field]
	if !ok || v == nil {
		return false
	}
	s, ok := v.(string)
	return ok && s != ""
}

// generatedFieldsOutput строит детерминированную проекцию `path → [поля]` для
// output/audit: ключи путей и поля внутри отсортированы; ТОЛЬКО имена, без
// значений секретов.
func generatedFieldsOutput(generated map[string][]string) map[string]any {
	out := make(map[string]any, len(generated))
	for path, fields := range generated {
		sorted := append([]string(nil), fields...)
		sort.Strings(sorted)
		out[path] = toAnySlice(sorted)
	}
	return out
}
