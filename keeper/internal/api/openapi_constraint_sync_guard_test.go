// Guard против drift между huma-struct-тегами OpenAPI-ограничений и
// АВТОРИТЕТНЫМИ рантайм-источниками валидации.
//
// ПРОБЛЕМА (см. delegation): huma-тег `pattern:"…"` / `minimum:"…"` /
// `maximum:"…"` принимает только строковый ЛИТЕРАЛ — нельзя сослаться на
// const operator.AIDPattern / rbac.RoleNamePattern. Значит литерал в теге и
// рантайм-const синхронятся ВРУЧНУЮ; при изменении одной стороны другая молча
// разойдётся, и собранная OpenAPI-спека начнёт врать о контракте.
//
// Этот тест извлекает значение тега РЕФЛЕКСИЕЙ по тем же op-input-структурам,
// которые huma компилирует в спеку (reflect.StructTag поля), и сверяет его
// ДОСЛОВНО с авторитетным рантайм-источником (const-паттерн или числовая
// граница доменного валидатора). Красный тест = тег устарел относительно
// рантайма (или наоборот).
//
// ТИРАЖ: перед возвратом ~145 ограничений каждое новое (поле, тег) ↔
// (рантайм-источник) добавляется одной строкой в constraintSyncCases ниже.
// Этот тест — стоп-инвариант массовой операции: пока пара не сверена с
// рантаймом, drift проходит молча.
package api

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Числовые границы ssh_target.ssh_port. Авторитетный рантайм-источник —
// доменный валидатор SoulHandler.UpdateSshTargetTyped (handlers/soul.go:1526:
// `req.SSHPort < 1 || req.SSHPort > 65535`). Отдельной const в коде нет:
// дублируем литералы здесь рядом со ссылкой, чтобы при смене границы валидатора
// тест краснел и заставлял синхронизировать тег.
const (
	sshPortRuntimeMin = "1"
	sshPortRuntimeMax = "65535"
)

// Числовые границы батч-параметров прогона (Voyage create/preview, Cadence
// create/patch). Авторитетные рантайм-источники — доменные валидаторы:
//   - voyage/cadence batch_size / concurrency / fail_threshold: `*v <= 0` →
//     минимум 1 (handlers/voyage.go:430/436/443, voyage/crud.go:107/110/119,
//     cadence/crud.go:116/122/125; все 422). Верхней границы у batch_size/
//     fail_threshold нет (handler НЕ 422-ит сверху) → тег только minimum.
//   - batch_percent: `< 1 || > 100` → [1, 100] (handlers/voyage.go:433,
//     voyage/crud.go:116, cadence/crud.go:119; 422).
//   - VOYAGE concurrency верх — voyageMaxConcurrency=500 (handlers/voyage.go:443:
//     `concurrency > voyageMaxConcurrency` → 422). CADENCE concurrency верх НЕ
//     ограничен рантаймом (cadence/crud.go валидирует только `<= 0`) → у Cadence
//     concurrency тег maximum НЕ ставится, чтобы не 422-ить принимаемое значение.
//
// Экспортной const у этих границ нет (литералы в валидаторах) — дублируем здесь
// рядом со ссылкой, как ssh_port в пилоте. Кандидаты на экспорт (voyage.MinBatchValue
// и т.п.), если границы начнут меняться.
const (
	batchValueRuntimeMin   = "1"   // batch_size / concurrency / fail_threshold: <= 0 → 422
	batchPercentRuntimeMin = "1"   // batch_percent: < 1 → 422
	batchPercentRuntimeMax = "100" // batch_percent: > 100 → 422
)

// voyageConcurrencyRuntimeMax — верхняя граница concurrency ТОЛЬКО для Voyage
// (handlers.voyageMaxConcurrency = 500, handlers/voyage.go:158/443). const
// неэкспортируемый (пакет handlers) — дублируем литерал. Кандидат на экспорт.
const voyageConcurrencyRuntimeMax = "500"

// ID-форматы output-полей (ТИРАЖ-БАТЧ 3: документационный pattern на машинно-
// генерируемых ID response-Body). Авторитетные источники без ЭКСПОРТНОГО const —
// дублируем литерал рядом со ссылкой (как ssh_port/batch выше); кандидаты на экспорт.
//
//   - ulidRuntimePattern: audit.NewULID → Crockford base32, 26 символов. Авторитет —
//     неэкспортируемый audit.ulidPattern (shared/audit/ulid.go:30, IsValidULID). Тот же
//     литерал уже несёт errandAccepted.ErrandID (huma_errand_accepted.go). Кандидат на
//     экспорт audit.ULIDPattern.
//   - sha256RuntimePattern: hex(sha256), lowercase 64 chars. Авторитет — hex.EncodeToString
//     над sha256.Sum256 (pluginhost/slot.go:173 для plugin-бинаря; keyservice.go:287
//     для key_id = SPKI-DER). Экспортной const нет (формат hex.EncodeToString). Кандидат
//     на экспорт sigil.SHA256HexPattern.
//
// SID/AID имеют ЭКСПОРТНЫЕ const → кейсы ссылаются на soul.SIDPattern / operator.AIDPattern
// напрямую (не дублируем литерал).
const (
	ulidRuntimePattern   = "^[0-9A-HJKMNP-TV-Z]{26}$" // = audit.ulidPattern (неэкспортируемый)
	sha256RuntimePattern = "^[0-9a-f]{64}$"           // = hex(sha256), lowercase 64 chars
)

// Рантайм-источники INPUT-паттернов БЕЗ экспортного const — дублируем литерал рядом
// со ссылкой (как ssh_port/batch выше); кандидаты на экспорт.
//
//   - sigilSegmentRuntimePattern: closed-charset path-сегментов Sigil (namespace/name/ref).
//     Авторитет — неэкспортируемый reSigilSegment (api/handlers/sigil.go:39 + mcp/
//     sigil_revoke.go:17, validateSigilTriple → 422 ДО svc.Allow/Revoke). ref здесь —
//     tag-ref ЭТИМ же валидатором (НЕ произвольный git-ref): слеш → 422. Кандидат на
//     экспорт sigil.SegmentPattern.
//   - choirNameRuntimePattern: имя Choir, kebab + `_`. Авторитет — неэкспортируемый
//     choir.choirNamePattern (choir.go:35, ValidChoirName); CreateTyped 422-ит ДО
//     INSERT. Handler инлайнит тот же литерал в текст ошибки (handlers/choir.go:156).
//     Кандидат на экспорт choir.NamePattern.
//   - soulPathRuntimePattern: soul_path обязан начинаться с `/` (абсолютный Unix-путь).
//     Рантайм — `req.SoulPath == "" || req.SoulPath[0] != '/'` → 422 (handlers/soul.go:1532).
//     Эквивалент = `^/` (start-anchor): пустая строка не матчит (422), голый `/` матчит
//     (рантайм его ПРИНИМАЕТ) — НЕ `^/.+`, иначе ложный 422 на валидном `/`.
const (
	sigilSegmentRuntimePattern = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" // = reSigilSegment (неэкспортируемый, ×2 копии)
	choirNameRuntimePattern    = "^[a-z][a-z0-9_-]*$"                 // = choir.choirNamePattern (неэкспортируемый)
	soulPathRuntimePattern     = "^/"                                 // = SoulPath[0]!='/' (start-with-slash, non-empty)
)

// constraintTagKind — какой huma-тег ограничения сверяется в кейсе.
type constraintTagKind string

const (
	tagPattern   constraintTagKind = "pattern"
	tagMinimum   constraintTagKind = "minimum"
	tagMaximum   constraintTagKind = "maximum"
	tagMinLength constraintTagKind = "minLength"
	tagMaxLength constraintTagKind = "maxLength"
)

// Length-границы (ТИРАЖ-БАТЧ 6). Авторитетные рантайм-источники без ЭКСПОРТНОГО
// const — дублируем литерал рядом со ссылкой (как ssh_port/batch выше).
//
//   - reasonRuntimeMinLen: unlock/rerun-last reason — рантайм 422-ит
//     `reason == ""` (UnlockTyped / RerunLastTyped; обе TypeValidationFailed → 422).
//   - covenRuntimeMaxLen: длина Coven-метки — soul.ValidCoven len>63 → 422
//     (soul.go:81, covenMaxLen=63). Тот же предел у declared-роли
//     (validHostRole len>63, incarnation.go:177) и у incarnation-имени через
//     pattern `{0,62}` (макс 63). Экспортной const у covenMaxLen нет (литерал
//     в ValidCoven) — кандидат на экспорт soul.CovenMaxLen.
//
// Верхняя граница reason — ЭКСПОРТНАЯ incarnation.ReasonMaxLen (=500): и тег
// maxLength, и рантайм-валидатор (UnlockTyped / RerunLastTyped 422-ят
// `len(reason) > ReasonMaxLen`) ссылаются на этот единый const, поэтому кейс
// runtime берёт его напрямую через strconv.Itoa (не литерал).
const (
	reasonRuntimeMinLen = "1"  // reason == "" → 422 (нижняя граница)
	covenRuntimeMaxLen  = "63" // ValidCoven / validHostRole len > 63 → 422
)

// sshUserRuntimeMinLen — ssh_user непустота. Рантайм 422-ит `req.SSHUser == ""`
// (UpdateSshTargetTyped handlers/soul.go:1529, TypeValidationFailed → 422).
// SoulSshTarget — class-A shared input↔output: minLength:1 на единой схеме
// (как committed-рукопись :6378); INPUT реально 422-ит пустое, OUTPUT doc-only.
const sshUserRuntimeMinLen = "1"

// synodDescRuntimeMin — Synod description непустота для PATCH. Рантайм
// UpdateTyped 422-ит `req.Description == ""` (handlers/synod.go). CreateTyped
// пустое ПРИНИМАЕТ (Description *string опц.) → minLength только на Update.
// Верхняя граница — ЭКСПОРТНАЯ rbac.SynodDescriptionMaxLen (=1024); CreateTyped
// и UpdateTyped оба 422-ят `len > SynodDescriptionMaxLen`.
const synodDescRuntimeMin = "1"

// constraintSyncCase — одна сверяемая пара «тег op-input-поля ↔ рантайм-источник».
//
//	structPtr  — указатель на zero-значение op-input-структуры (для reflect.Type).
//	fieldPath  — путь к полю: одно имя ("AID") или цепочка через embedded/Body
//	             ("Body", "AID"). Совпадает с тем, как huma спускается по структуре.
//	tag        — какой именно тег ограничения читать.
//	runtime    — ожидаемое значение: дословно const-паттерн или числовая граница
//	             доменного рантайм-валидатора. ИСТОЧНИК ПРАВДЫ — рантайм, не тег.
//	source     — человекочитаемая ссылка на рантайм-источник для текста ошибки.
type constraintSyncCase struct {
	name      string
	structPtr any
	fieldPath []string
	tag       constraintTagKind
	runtime   string
	source    string
}

// constraintSyncCases — реестр пар. ТИРАЖ: добавляй сюда строку на каждое
// возвращаемое в спеку ограничение. Если рантайм-источник — новый const,
// импортируй его и сошлись на него в runtime (не копируй литерал руками,
// если const доступен из этого пакета).
//
// Покрыто на текущем коде (пилотные теги):
//   - AID на operator.create / operator.get / operator.revoke /
//     operator.issue-token (×4 поля, все ← operator.AIDPattern);
//   - AID на role.revoke-operator (path) и GrantOperatorRequest.AID (body,
//     общий тип для role.grant-operator + synod.add-operator) ← operator.AIDPattern;
//   - role name на role.create ← rbac.RoleNamePattern;
//   - ssh_port minimum/maximum на soul.ssh-target ← границы UpdateSshTargetTyped.
var constraintSyncCases = []constraintSyncCase{
	// --- AID-паттерн (operator.AIDPattern) ---
	{
		name:      "operator.create AID",
		structPtr: &operatorCreateInput{},
		fieldPath: []string{"Body", "AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.get AID (path)",
		structPtr: &operatorGetInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.revoke AID (path)",
		structPtr: &operatorRevokeInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.issue-token AID (path)",
		structPtr: &operatorIssueTokenInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "role.revoke-operator AID (path)",
		structPtr: &roleRevokeOperatorInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "GrantOperatorRequest AID (role.grant-operator / synod.add-operator body)",
		structPtr: &GrantOperatorRequest{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},

	// --- role-name-паттерн (rbac.RoleNamePattern) ---
	{
		name:      "role.create name",
		structPtr: &roleCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern",
	},

	// --- ssh_port границы (UpdateSshTargetTyped, handlers/soul.go:1526) ---
	{
		name:      "soul.ssh-target ssh_port minimum",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHPort"},
		tag:       tagMinimum,
		runtime:   sshPortRuntimeMin,
		source:    "SoulHandler.UpdateSshTargetTyped (ssh_port >= 1)",
	},
	{
		name:      "soul.ssh-target ssh_port maximum",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHPort"},
		tag:       tagMaximum,
		runtime:   sshPortRuntimeMax,
		source:    "SoulHandler.UpdateSshTargetTyped (ssh_port <= 65535)",
	},

	// --- kebab-имя name/on_beacon (oracle.NamePattern ^[a-z0-9-]{1,63}$) ---
	// Авторитет — oracle.NamePattern (oracle/validate.go); тот же литерал несут
	// augur.NamePattern / herald.NamePattern (см. ниже). Каждый источник свой —
	// сверяем поле с ЕГО доменным валидатором, не с чужим однотипным.
	{
		name:      "vigil.create name",
		structPtr: &vigilCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (CreateVigilTyped)",
	},
	{
		name:      "decree.create name",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (CreateDecreeTyped)",
	},
	{
		name:      "decree.create on_beacon",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "OnBeacon"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (on_beacon = Vigil name, CreateDecreeTyped)",
	},
	{
		name:      "omen.create name",
		structPtr: &omenCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (CreateOmenTyped)",
	},
	{
		name:      "herald.create name",
		structPtr: &heraldCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (CreateHeraldTyped)",
	},
	{
		name:      "tiding.create name",
		structPtr: &tidingCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (CreateTidingTyped)",
	},
	{
		name:      "VoyageNotify herald (voyage.create / cadence.create notify[].herald)",
		structPtr: &VoyageNotify{},
		fieldPath: []string{"Herald"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (prepareNotifyTidingsErr, voyage_notify.go:103)",
	},

	// --- service/synod-имя (^[a-z][a-z0-9-]*$) ---
	// service name — собственный serviceregistry.NamePattern (НЕ rbac.RoleNamePattern,
	// хоть литерал совпадает: разные домены, разные SQL CHECK). synod name — reRoleName
	// (rbac.RoleNamePattern), единый с role name по решению synod.go.
	{
		name:      "service.register name",
		structPtr: &serviceRegisterInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   serviceregistry.NamePattern,
		source:    "serviceregistry.NamePattern (CreateService)",
	},
	{
		name:      "synod.create name",
		structPtr: &synodCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (reRoleName, CreateSynod)",
	},

	// --- coven label (soul.CovenPattern ^[a-z][a-z0-9]*(-[a-z0-9]+)*$) ---
	// covens[] / labels[] — per-element ValidCoven (пустой элемент отвергают и huma-
	// pattern, и ValidCoven → match). label (append/remove) и selector.coven НЕ
	// тегируются: label-валидность зависит от mode (replace требует ПУСТОЙ label,
	// pattern бы ложно 422-ил валидный replace), selector.coven — фильтр.
	{
		name:      "soul.create covens[]",
		structPtr: &soulCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (CreateTyped, soul.go:221)",
	},
	{
		name:      "soul.coven-assign labels[]",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Labels"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (replace-mode per-element, soul.go:1253)",
	},
	{
		name:      "incarnation.create covens[]",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (incarnation CreateTyped, incarnation_typed.go:95)",
	},

	// --- timeout_seconds верх (errand.MaxTimeoutSeconds = 300) ---
	// ErrandRunRequest.timeout_seconds: рантайм 422-ит ТОЛЬКО `> MaxTimeoutSeconds`
	// (dispatcher.go:685, dispatchError → ErrTimeoutOutOfRange). НИЖНЕЙ границы тегом
	// НЕТ: timeout_seconds=0 (или опущено) → DefaultTimeoutSeconds (dispatcher.go:683,
	// валидно) → minimum:"1" ложно 422-ил бы валидный 0 (ловушка нуля). Источник —
	// ЭКСПОРТНЫЙ const errand.MaxTimeoutSeconds (не литерал).
	{
		name:      "errand exec timeout_seconds maximum",
		structPtr: &errandExecInput{},
		fieldPath: []string{"Body", "TimeoutSeconds"},
		tag:       tagMaximum,
		runtime:   strconv.Itoa(errand.MaxTimeoutSeconds),
		source:    "errand.MaxTimeoutSeconds (dispatcher.go:685, ErrTimeoutOutOfRange → 422)",
	},

	// --- батч-границы Voyage create (handlers/voyage.go + voyage/crud.go) ---
	// Все поля *int omitempty: nil/опущено → дефолт (без 422); явный 0/<1 → 422 (тот
	// же предикат, что huma minimum). batch_size/fail_threshold — только minimum
	// (верх не валидируется). batch_percent — [1,100]. concurrency — [1,500] (верх
	// voyageMaxConcurrency, ТОЛЬКО Voyage).
	{
		name:      "voyage.create batch_size minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:430, batch_size > 0)",
	},
	{
		name:      "voyage.create batch_percent minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:433, batch_percent in [1,100])",
	},
	{
		name:      "voyage.create batch_percent maximum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "validateVoyageRequest (handlers/voyage.go:433, batch_percent in [1,100])",
	},
	{
		name:      "voyage.create concurrency minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:443, concurrency in [1,500])",
	},
	{
		name:      "voyage.create concurrency maximum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMaximum,
		runtime:   voyageConcurrencyRuntimeMax,
		source:    "validateVoyageRequest (handlers/voyage.go:443, concurrency <= voyageMaxConcurrency=500)",
	},
	{
		name:      "voyage.create fail_threshold minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:436, fail_threshold > 0)",
	},

	// --- батч-границы Cadence create (cadence/crud.go validate, через Insert) ---
	// concurrency верх у Cadence НЕ ограничен рантаймом → maximum НЕ тегируем.
	{
		name:      "cadence.create batch_size minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:116, batch_size > 0)",
	},
	{
		name:      "cadence.create batch_percent minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:119, batch_percent in [1,100])",
	},
	{
		name:      "cadence.create batch_percent maximum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "cadence.validate (cadence/crud.go:119, batch_percent in [1,100])",
	},
	{
		name:      "cadence.create concurrency minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:122, concurrency > 0)",
	},
	{
		name:      "cadence.create fail_threshold minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:125, fail_threshold > 0)",
	},

	// --- батч-границы Cadence PATCH (тот же cadence.validate через Update) ---
	{
		name:      "cadence.patch batch_size minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:116 via Update, batch_size > 0)",
	},
	{
		name:      "cadence.patch batch_percent minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:119 via Update, batch_percent in [1,100])",
	},
	{
		name:      "cadence.patch batch_percent maximum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "cadence.validate (cadence/crud.go:119 via Update, batch_percent in [1,100])",
	},
	{
		name:      "cadence.patch concurrency minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:122 via Update, concurrency > 0)",
	},
	{
		name:      "cadence.patch fail_threshold minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:125 via Update, fail_threshold > 0)",
	},

	// ====================================================================
	// ID-ФОРМАТЫ OUTPUT-ПОЛЕЙ (ТИРАЖ-БАТЧ 3). Документационный pattern: huma НЕ
	// валидирует response-body (эмпирически 200, не 500) → тег чисто документация
	// формата для клиент-кодогена. Цель кейса — «документированный формат ==
	// канонический рантайм-источник генератора/валидатора». structPtr — ЭКСПОРТНЫЙ
	// reply-struct напрямую (fieldPath = имя поля, БЕЗ Body-обёртки — output-Body это
	// сама структура). Для []string-полей (covens-стиль) pattern сидит на items[].
	// ====================================================================

	// --- ULID на машинно-генерируемых ID (audit.NewULID) ---
	{
		name:      "errandAccepted errand_id ULID",
		structPtr: &errandAccepted{},
		fieldPath: []string{"ErrandID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (errand, dispatcher.go:262)",
	},
	{
		name:      "ErrandResult errand_id ULID",
		structPtr: &ErrandResult{},
		fieldPath: []string{"ErrandID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (errand, dispatcher.go:262)",
	},
	{
		name:      "IncarnationCreateReply apply_id ULID",
		structPtr: &IncarnationCreateReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation_typed.go:163)",
	},
	{
		name:      "IncarnationRunReply apply_id ULID",
		structPtr: &IncarnationRunReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation_typed.go:265)",
	},
	{
		name:      "IncarnationUpgradeReply apply_id ULID",
		structPtr: &IncarnationUpgradeReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation upgrade)",
	},
	{
		name:      "IncarnationRerunLastReply apply_id ULID",
		structPtr: &IncarnationRerunLastReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation rerun-last)",
	},
	{
		name:      "IncarnationDestroyReply apply_id ULID",
		structPtr: &IncarnationDestroyReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation destroy)",
	},
	{
		name:      "StateHistoryEntry apply_id ULID",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (state_history.apply_id)",
	},
	{
		name:      "StateHistoryEntry history_id ULID",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"HistoryID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 006, history_id)",
	},
	{
		name:      "PushApplyReply apply_id ULID",
		structPtr: &PushApplyReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "PushApplyView apply_id ULID",
		structPtr: &PushApplyView{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "PushRunListEntry apply_id ULID",
		structPtr: &PushRunListEntry{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "Voyage voyage_id ULID",
		structPtr: &Voyage{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageTargetsReply voyage_id ULID",
		structPtr: &VoyageTargetsReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageCreateReply voyage_id ULID",
		structPtr: &VoyageCreateReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageCancelReply voyage_id ULID",
		structPtr: &VoyageCancelReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "CadenceCreateReply cadence_id ULID",
		structPtr: &CadenceCreateReply{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "CadenceEnabledReply cadence_id ULID",
		structPtr: &CadenceEnabledReply{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "cadence (element) cadence_id ULID",
		structPtr: &cadence{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "SoulHistoryItem id ULID",
		structPtr: &SoulHistoryItem{},
		fieldPath: []string{"ID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (apply_id|errand_id, soul/history.go:55)",
	},
	{
		name:      "SoulHistoryItem voyage_id ULID",
		structPtr: &SoulHistoryItem{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (voyage_id)",
	},
	{
		name:      "AuditEvent id ULID",
		structPtr: &AuditEvent{},
		fieldPath: []string{"ID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 001, audit_id)",
	},
	{
		name:      "AuditEvent correlation_id ULID",
		structPtr: &AuditEvent{},
		fieldPath: []string{"CorrelationID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 001, correlation_id)",
	},

	// --- sha256 hex на хэш-производных ID ---
	{
		name:      "PluginSigilAllowReply sha256 hex",
		structPtr: &PluginSigilAllowReply{},
		fieldPath: []string{"SHA256"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256) бинаря (pluginhost/slot.go:173)",
	},
	{
		name:      "PluginSigilView sha256 hex",
		structPtr: &PluginSigilView{},
		fieldPath: []string{"SHA256"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256) бинаря (pluginhost/slot.go:173)",
	},
	{
		name:      "SigilKeyIntroduceReply key_id hex",
		structPtr: &SigilKeyIntroduceReply{},
		fieldPath: []string{"KeyID"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256(SPKI-DER)) (keyservice.go:287)",
	},
	{
		name:      "SigilKeyView key_id hex",
		structPtr: &SigilKeyView{},
		fieldPath: []string{"KeyID"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256(SPKI-DER)) (keyservice.go:287)",
	},

	// --- SID на sid-output-полях (← soul.SIDPattern) ---
	{
		name:      "ErrandResult sid",
		structPtr: &ErrandResult{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulCreateReply sid",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulIssueTokenReply sid",
		structPtr: &SoulIssueTokenReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulSshTargetReply sid",
		structPtr: &SoulSshTargetReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulListEntry sid",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulHistoryReply sid",
		structPtr: &SoulHistoryReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "Voice sid",
		structPtr: &Voice{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},

	// --- AID на output *_by_aid / aid / archon_aid / operators[] (← operator.AIDPattern) ---
	// Безопасно: huma НЕ валидирует output (200, не 500); миграция 058 — текущий AID-
	// паттерн надмножество старого, легаси-AID тоже матчатся.
	{
		name:      "OperatorCreateReply aid",
		structPtr: &OperatorCreateReply{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "OperatorCreateReply created_by_aid",
		structPtr: &OperatorCreateReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Operator aid",
		structPtr: &Operator{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Operator created_by_aid",
		structPtr: &Operator{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IssueTokenReply aid",
		structPtr: &IssueTokenReply{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IncarnationUnlockReply unlocked_by_aid",
		structPtr: &IncarnationUnlockReply{},
		fieldPath: []string{"UnlockedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IncarnationGetReply created_by_aid",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "StateHistoryEntry changed_by_aid",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"ChangedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Voyage started_by_aid",
		structPtr: &Voyage{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "cadence (element) created_by_aid",
		structPtr: &cadence{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushApplyView started_by_aid",
		structPtr: &PushApplyView{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushRunListEntry started_by_aid",
		structPtr: &PushRunListEntry{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushProvider created_by_aid",
		structPtr: &PushProvider{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushProvider updated_by_aid",
		structPtr: &PushProvider{},
		fieldPath: []string{"UpdatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PluginSigilView allowed_by_aid",
		structPtr: &PluginSigilView{},
		fieldPath: []string{"AllowedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SoulCreateReply created_by_aid",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SoulListEntry created_by_aid",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Choir created_by_aid",
		structPtr: &Choir{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Voice added_by_aid",
		structPtr: &Voice{},
		fieldPath: []string{"AddedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "AuditEvent archon_aid",
		structPtr: &AuditEvent{},
		fieldPath: []string{"ArchonAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SynodView operators[] AID (per-element)",
		structPtr: &SynodView{},
		fieldPath: []string{"Operators"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern (member AID)",
	},

	// ====================================================================
	// INPUT-ПАТТЕРНЫ С РАНТАЙМ-422 (ТИРАЖ-БАТЧ 4). Каждое поле рантайм 422-ит на
	// неверный ФОРМАТ ДО любых иных проверок (existence/FK/whitelist → 400/404/500
	// тегом НЕ покрываются). Body-поля — через Body-обёртку; path-параметры — прямо.
	// ====================================================================

	// --- Sigil тройка namespace/name/ref (reSigilSegment, 422 в validateSigilTriple) ---
	// ref — tag-ref ЭТИМ же валидатором (НЕ произвольный git-ref): слеш → 422.
	{
		name:      "sigil.allow namespace",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Namespace"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped)",
	},
	{
		name:      "sigil.allow name",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped)",
	},
	{
		name:      "sigil.allow ref",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Ref"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped — tag-ref, слеш→422)",
	},
	{
		name:      "sigil.revoke namespace (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Namespace"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped)",
	},
	{
		name:      "sigil.revoke name (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped)",
	},
	{
		name:      "sigil.revoke ref (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Ref"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped — tag-ref, слеш→422)",
	},

	// --- incarnation name/service (incarnation.NamePattern, 422 в CreateTyped) ---
	// service ВАЛИДИРУЕТСЯ тем же incarnation.NamePattern (handler reuse, не
	// serviceregistry.NamePattern) и 422-ит ДО service-resolve (FK → 422 «not registered»
	// идёт ПОЗЖЕ, формат-422 раньше). НЕ coven-паттерн.
	{
		name:      "incarnation.create name",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (CreateTyped, incarnation_typed.go:85)",
	},
	{
		name:      "incarnation.create service",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Service"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (CreateTyped service-format, incarnation_typed.go:91)",
	},

	// --- push-provider name (pushprovider.NamePattern, 422 ДО existence) ---
	// create body + get/update/delete path — все 422-ят формат ДО ErrAlreadyExists/404.
	{
		name:      "push-provider.create name",
		structPtr: &pushProviderCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (CreateTyped, pushprovider.go:141)",
	},
	{
		name:      "push-provider.get name (path)",
		structPtr: &pushProviderGetInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (GetTyped, pushprovider.go:271)",
	},
	{
		name:      "push-provider.update name (path)",
		structPtr: &pushProviderUpdateInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (UpdateTyped, pushprovider.go:178)",
	},
	{
		name:      "push-provider.delete name (path)",
		structPtr: &pushProviderDeleteInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (DeleteTyped, pushprovider.go:219)",
	},

	// --- choir_name (choir.choirNamePattern, 422 ДО INSERT) ---
	{
		name:      "choir.create choir_name",
		structPtr: &choirCreateInput{},
		fieldPath: []string{"Body", "ChoirName"},
		tag:       tagPattern,
		runtime:   choirNameRuntimePattern,
		source:    "choir.ValidChoirName (CreateTyped, choir.go:154)",
	},

	// --- Voice sid (soul.SIDPattern, 422 ДО membership-check) ---
	// add-voice body sid + remove-voice path sid — формат 422 раньше «SID не член».
	{
		name:      "voice.add sid",
		structPtr: &voiceAddInput{},
		fieldPath: []string{"Body", "SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern (AddVoiceTyped, choir.go:287)",
	},
	{
		name:      "voice.remove sid (path)",
		structPtr: &voiceRemoveInput{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern (RemoveVoiceTyped, choir.go:399)",
	},

	// --- decree incarnation_name/action_scenario (oracle.*Pattern, 422 ДО INSERT) ---
	{
		name:      "decree.create incarnation_name",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "IncarnationName"},
		tag:       tagPattern,
		runtime:   oracle.IncarnationPattern,
		source:    "oracle.IncarnationPattern (CreateDecree, service.go:179)",
	},
	{
		name:      "decree.create action_scenario",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "ActionScenario"},
		tag:       tagPattern,
		runtime:   oracle.ScenarioPattern,
		source:    "oracle.ScenarioPattern (CreateDecree, service.go:182)",
	},

	// --- soul_path абсолютность (start-with-slash, 422 в UpdateSshTargetTyped) ---
	// SoulSshTarget — class-A shared input↔output; pattern документирует ОБА (output
	// soul_path из БД всегда `/`-абсолютный, тем же валидатором при записи). `^/` (НЕ
	// `^/.+`): голый `/` рантайм ПРИНИМАЕТ, `^/.+` бы ложно 422-ил.
	{
		name:      "soul.ssh-target soul_path",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SoulPath"},
		tag:       tagPattern,
		runtime:   soulPathRuntimePattern,
		source:    "UpdateSshTargetTyped (soul.go:1532, SoulPath[0]!='/' → 422)",
	},

	// ====================================================================
	// ПАТТЕРНЫ ИМЁН OUTPUT-ПОЛЕЙ (ТИРАЖ-БАТЧ 5). Документационный pattern: huma НЕ
	// валидирует response-body (как ID-форматы батча 3) → тег чисто документация формата
	// для клиент-кодогена. Цель кейса — «документированный формат имени == канонический
	// рантайм-источник (тот же const, что для одноимённого INPUT-поля)». structPtr —
	// reply/view-struct напрямую (fieldPath = имя поля, БЕЗ Body-обёртки — output-Body это
	// сама структура). Для []string-полей (covens/roles/labels стиль) pattern сидит на items[].
	// Все эти типы output-only (request-Body — отдельные *Request/*Input) → input-422-риска нет.
	// ====================================================================

	// --- kebab-имя name/omen/on_beacon (oracle/augur/herald.NamePattern ^[a-z0-9-]{1,63}$) ---
	{
		name:      "OmenView name (augur.NamePattern)",
		structPtr: &OmenView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (output omen name)",
	},
	{
		name:      "RiteView omen (augur.NamePattern, FK)",
		structPtr: &RiteView{},
		fieldPath: []string{"Omen"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (output rite.omen — FK на omens.name)",
	},
	{
		name:      "VigilView name (oracle.NamePattern)",
		structPtr: &VigilView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output vigil name)",
	},
	{
		name:      "DecreeView name (oracle.NamePattern)",
		structPtr: &DecreeView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output decree name)",
	},
	{
		name:      "DecreeView on_beacon (oracle.NamePattern, FK на Vigil)",
		structPtr: &DecreeView{},
		fieldPath: []string{"OnBeacon"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output decree.on_beacon — имя Vigil-а)",
	},
	{
		name:      "Herald name (herald.NamePattern)",
		structPtr: &Herald{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output herald name)",
	},
	{
		name:      "Tiding name (herald.NamePattern)",
		structPtr: &Tiding{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output tiding name)",
	},
	{
		name:      "Tiding herald (herald.NamePattern, FK)",
		structPtr: &Tiding{},
		fieldPath: []string{"Herald"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output tiding.herald — FK на heralds.name)",
	},

	// --- role-name (rbac.RoleNamePattern ^[a-z][a-z0-9-]*$) ---
	// RoleView.name + SynodView.name (синод-имя единым reRoleName) + SynodView.roles[]
	// (per-element имена ролей). RoleView.operators[]/SynodView.operators[] — AID, НЕ name.
	{
		name:      "RoleView name (rbac.RoleNamePattern)",
		structPtr: &RoleView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output role name)",
	},
	{
		name:      "SynodView name (rbac.RoleNamePattern, reRoleName)",
		structPtr: &SynodView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output synod name — единый reRoleName)",
	},
	{
		name:      "SynodView roles[] (rbac.RoleNamePattern, per-element)",
		structPtr: &SynodView{},
		fieldPath: []string{"Roles"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output synod.roles[] — имена ролей)",
	},

	// --- service name (serviceregistry.NamePattern ^[a-z][a-z0-9-]*$) ---
	{
		name:      "ServiceView name (serviceregistry.NamePattern)",
		structPtr: &ServiceView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   serviceregistry.NamePattern,
		source:    "serviceregistry.NamePattern (output service name)",
	},

	// --- coven label (soul.CovenPattern ^[a-z][a-z0-9]*(-[a-z0-9]+)*$, per-element) ---
	// output covens[]/labels[] в Soul*/Incarnation* View/Reply. *[]string и []string —
	// reflect-тег читается одинаково (helper спускается по Field, slice-обёртка прозрачна).
	{
		name:      "SoulCreateReply covens[] (soul.CovenPattern)",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "SoulListEntry covens[] (soul.CovenPattern)",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "IncarnationGetReply covens[] (soul.CovenPattern)",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "soulCovenAssignReply labels[] (soul.CovenPattern, replace-эхо)",
		structPtr: &soulCovenAssignReply{},
		fieldPath: []string{"Labels"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output replace labels[], per-element)",
	},

	// --- incarnation_name (incarnation.NamePattern ^[a-z0-9][a-z0-9-]{0,62}$) ---
	// IncarnationGetReply.name + echo Incarnation в create/run/rerun-last + choir/voice
	// incarnation_name. DecreeView.incarnation_name — отдельный const oracle.IncarnationPattern
	// (значение идентично, но домен decree — сверяем с ЕГО источником).
	{
		name:      "IncarnationGetReply name (incarnation.NamePattern)",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation name)",
	},
	{
		name:      "IncarnationCreateReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationCreateReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "IncarnationRunReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationRunReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "IncarnationRerunLastReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationRerunLastReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "Choir incarnation_name (incarnation.NamePattern)",
		structPtr: &Choir{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output choir.incarnation_name — FK)",
	},
	{
		name:      "Voice incarnation_name (incarnation.NamePattern)",
		structPtr: &Voice{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output voice.incarnation_name — FK)",
	},
	{
		name:      "DecreeView incarnation_name (oracle.IncarnationPattern)",
		structPtr: &DecreeView{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   oracle.IncarnationPattern,
		source:    "oracle.IncarnationPattern (output decree.incarnation_name — тот же const, что INPUT)",
	},

	// ====================================================================
	// LENGTH-ГРАНИЦЫ INPUT (ТИРАЖ-БАТЧ 6 + добивка). minLength/maxLength, где
	// рантайм РЕАЛЬНО 422-ит ту же границу. reason — обе границы: непустота
	// (minLength:1) + верх incarnation.ReasonMaxLen (maxLength:500, рантайм-
	// валидатор UnlockTyped/RerunLastTyped, решение PM вариант (а)).
	// ====================================================================

	// --- reason непустота + верх ReasonMaxLen (UnlockTyped/RerunLastTyped → 422) ---
	{
		name:      "incarnation.unlock reason minLength",
		structPtr: &incUnlockInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMinLength,
		runtime:   reasonRuntimeMinLen,
		source:    "UnlockTyped (incarnation_typed.go, reason == \"\" → 422)",
	},
	{
		name:      "incarnation.unlock reason maxLength",
		structPtr: &incUnlockInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(incarnation.ReasonMaxLen),
		source:    "incarnation.ReasonMaxLen (UnlockTyped, len(reason) > 500 → 422)",
	},
	{
		name:      "incarnation.rerun-last reason minLength",
		structPtr: &incRerunInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMinLength,
		runtime:   reasonRuntimeMinLen,
		source:    "RerunLastTyped (incarnation_typed.go, reason == \"\" → 422)",
	},
	{
		name:      "incarnation.rerun-last reason maxLength",
		structPtr: &incRerunInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(incarnation.ReasonMaxLen),
		source:    "incarnation.ReasonMaxLen (RerunLastTyped, len(reason) > 500 → 422)",
	},

	// --- ssh_user непустота (UpdateSshTargetTyped soul.go:1529, "" → 422) ---
	// SoulSshTarget — class-A shared input↔output: INPUT 422-ит пустое, OUTPUT
	// doc-only (huma выходы не валидирует). Кейс сверяет ЕДИНУЮ схему.
	{
		name:      "soul.ssh-target ssh_user minLength",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHUser"},
		tag:       tagMinLength,
		runtime:   sshUserRuntimeMinLen,
		source:    "UpdateSshTargetTyped (handlers/soul.go:1529, ssh_user == \"\" → 422)",
	},

	// --- coven/role длина 63 (ValidCoven / validHostRole len>63 → 422) ---
	// maxLength на []string-полях сидит на items (covens/labels). Нижней границы
	// тегом НЕТ: пустой role/coven/incarnation-selector валиден (opt/no-op),
	// minLength:1 ложно 422-ил бы валидное пустое.
	{
		// maxLength сидит на вложенном IncarnationSpecHost.Role (elem
		// PATCH .../hosts body.hosts[]) — ссылаемся на элемент-структуру
		// напрямую (constraintTag не входит внутрь slice-элемента).
		name:      "IncarnationSpecHost role maxLength",
		structPtr: &IncarnationSpecHost{},
		fieldPath: []string{"Role"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "validHostRole (handlers/incarnation.go:177, len(role) > 63 → 422)",
	},
	{
		name:      "soul.create covens[] maxLength",
		structPtr: &soulCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (soul.go:81, len(label) > 63 → 422; CreateTyped soul.go:221)",
	},
	{
		// Симметрия с soul.create covens[]: рантайм incarnation CreateTyped
		// уже 422-ит len>63 per-element (ValidCoven, incarnation_typed.go:95) —
		// тег чисто восстанавливает границу в спеке.
		name:      "incarnation.create covens[] maxLength",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (incarnation CreateTyped, incarnation_typed.go:95, len(label) > 63 → 422)",
	},
	{
		name:      "soul.coven-assign label maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Label"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (append/remove, soul.go:1239; пустой замен при replace → valid)",
	},
	{
		name:      "soul.coven-assign labels[] maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Labels"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (replace per-element, soul.go:1253)",
	},
	{
		name:      "soul.coven-assign selector.coven maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Selector", "Coven"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (selector.coven != \"\", soul.go:1281)",
	},
	{
		name:      "soul.coven-assign selector.incarnation maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Selector", "Incarnation"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "incarnation.ValidName pattern {0,62}=63 (selector.incarnation != \"\", soul.go:1284)",
	},

	// --- synod description (rbac.SynodDescriptionMaxLen=1024) ---
	// CreateTyped: description опц. (*string) → только maxLength. UpdateTyped:
	// description обязателен, == "" → 422 → minLength:1 + maxLength:1024.
	{
		name:      "synod.create description maxLength",
		structPtr: &synodCreateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(rbac.SynodDescriptionMaxLen),
		source:    "rbac.SynodDescriptionMaxLen (CreateTyped synod.go:133, len > 1024 → 422)",
	},
	{
		name:      "synod.update description minLength",
		structPtr: &synodUpdateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMinLength,
		runtime:   synodDescRuntimeMin,
		source:    "UpdateTyped (synod.go, description == \"\" → 422)",
	},
	{
		name:      "synod.update description maxLength",
		structPtr: &synodUpdateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(rbac.SynodDescriptionMaxLen),
		source:    "rbac.SynodDescriptionMaxLen (UpdateTyped synod.go, len > 1024 → 422)",
	},
}

// TestOpenAPIConstraintSyncWithRuntime — каждый кейс: тег op-input-поля обязан
// ДОСЛОВНО совпасть с авторитетным рантайм-источником. Расхождение = спека
// врёт о контракте → красный тест.
func TestOpenAPIConstraintSyncWithRuntime(t *testing.T) {
	for _, c := range constraintSyncCases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := constraintTag(t, c.structPtr, c.fieldPath, c.tag)
			if !ok {
				t.Fatalf("поле %v типа %s НЕ несёт тега %q — пилотное ограничение исчезло из спеки (drift спека<рантайм)",
					c.fieldPath, reflect.TypeOf(c.structPtr).Elem().Name(), c.tag)
			}
			if got != c.runtime {
				t.Fatalf("DRIFT тег<>рантайм для %s:\n  huma-тег %q = %q\n  рантайм-источник %s = %q\n→ синхронизируй литерал тега с рантайм-валидатором (ручная синхронизация, см. шапку файла)",
					c.name, c.tag, got, c.source, c.runtime)
			}
		})
	}
}

// constraintTag спускается по fieldPath в структуре structPtr и возвращает
// значение запрошенного тега ограничения у конечного поля. Путь повторяет то,
// как huma рекурсивно обходит вложенные/Body-структуры op-input-а.
func constraintTag(t *testing.T, structPtr any, fieldPath []string, kind constraintTagKind) (string, bool) {
	t.Helper()
	typ := reflect.TypeOf(structPtr)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	var last reflect.StructField
	for i, name := range fieldPath {
		if typ.Kind() != reflect.Struct {
			t.Fatalf("сегмент %q пути %v: предыдущий тип %s не структура", name, fieldPath, typ.Name())
		}
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("поле %q не найдено в %s (путь %v) — структура op-input переименована? обнови кейс",
				name, typ.Name(), fieldPath)
		}
		last = f
		if i < len(fieldPath)-1 {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			typ = ft
		}
	}
	return last.Tag.Lookup(string(kind))
}
