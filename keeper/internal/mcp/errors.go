package mcp

import (
	"errors"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// MCP error code suffix-ы из docs/keeper/mcp-tools.md → § Errors.
// Стабильные suffix-ы URN-ов Operator API (`https://soul-stack.io/errors/<suffix>`),
// прокинутые в MCP-error `data.code`. Источник правды — operator-api.md →
// «Типы ошибок».
const (
	mcpCodeUnauthenticated     = "unauthenticated"
	mcpCodeForbidden           = "forbidden"
	mcpCodeNotFound            = "not-found"
	mcpCodeValidationFailed    = "validation-failed"
	mcpCodeMalformedRequest    = "malformed-request"
	mcpCodeOperatorExists      = "operator-already-exists"
	mcpCodeOperatorRevoked     = "operator-revoked"
	mcpCodeWouldLockOutCluster = "would-lock-out-cluster"
	mcpCodeInternalError       = "internal-error"
	mcpCodeNotImplemented      = "not-implemented"

	// Incarnation-коды из docs/keeper/mcp-tools.md → § Errors (стабильные
	// URN-suffix-ы). incarnation-locked покрывает все state-конфликты
	// ресурса (error_locked / busy / downgrade / schema-mismatch — REST
	// маппит их в один problem-type TypeIncarnationLocked).
	mcpCodeIncarnationExists = "incarnation-already-exists"
	mcpCodeIncarnationLocked = "incarnation-locked"

	// Role-коды (RBAC-CRUD, Slice 2b). role-already-exists — UNIQUE-violation
	// на rbac_roles.name; role-builtin — попытка delete/update встроенной роли
	// (cluster-admin). would-lock-out-cluster переиспользует существующий
	// mcpCodeWouldLockOutCluster (общий problem-type для operator и role
	// self-lockout). not-found / validation-failed / forbidden — общие коды.
	mcpCodeRoleExists  = "role-already-exists"
	mcpCodeRoleBuiltin = "role-builtin"

	// Synod-коды (ADR-049, паритет REST /v1/synods*). synod-already-exists —
	// UNIQUE-violation на synods.name (REST TypeSynodExists); synod-not-found —
	// группы нет (REST TypeSynodNotFound); synod-builtin — synod.delete над
	// builtin (REST TypeSynodBuiltin). would-lock-out-cluster / not-found /
	// validation-failed / forbidden — общие коды (как у role-tools).
	mcpCodeSynodExists   = "synod-already-exists"
	mcpCodeSynodNotFound = "synod-not-found"
	mcpCodeSynodBuiltin  = "synod-builtin"

	// Soul-коды (онбординг, паритет REST POST /v1/souls + issue-token).
	// soul-already-exists — UNIQUE-violation на souls.sid (REST TypeSoulExists);
	// bootstrap-token-active — на SID уже висит активный bootstrap-токен, force
	// не указан (REST TypeBootstrapTokenActive). not-found / validation-failed /
	// forbidden — общие коды.
	mcpCodeSoulExists           = "soul-already-exists"
	mcpCodeBootstrapTokenActive = "bootstrap-token-active"

	// Sigil-коды (plugin allow-list, S4b — паритет REST POST/DELETE
	// /v1/plugins/sigils*). plugin-not-in-cache — плагина (ns, name) нет в
	// single-slot кеше host-а (REST TypePluginNotInCache, 404); sigil-already-
	// active — активный допуск на (ns, name, ref) уже есть (REST TypeSigilActive,
	// 409); sigil-not-found — активной записи для revoke нет (REST
	// TypeSigilNotFound, 404). validation-failed / forbidden — общие коды.
	mcpCodePluginNotInCache = "plugin-not-in-cache"
	mcpCodeSigilActive      = "sigil-already-active"
	mcpCodeSigilNotFound    = "sigil-not-found"

	// Sigil-key-коды (ротация ключей подписи, R3-S7 — паритет REST
	// /v1/sigil/keys*). sigil-key-not-found — ключа нет (REST TypeSigilKeyNotFound,
	// 404); sigil-key-last-active — последний active (REST TypeSigilKeyLastActive,
	// 409); sigil-key-primary — retire primary напрямую (REST TypeSigilKeyPrimary,
	// 409); sigil-key-concurrent-change — гонка primary / retired-ключ на
	// set-primary (REST TypeSigilKeyConcurrentChange, 409).
	mcpCodeSigilKeyNotFound         = "sigil-key-not-found"
	mcpCodeSigilKeyLastActive       = "sigil-key-last-active"
	mcpCodeSigilKeyPrimary          = "sigil-key-primary"
	mcpCodeSigilKeyConcurrentChange = "sigil-key-concurrent-change"

	// Service-код (реестр Service-ов, ADR-028 S3 — паритет REST POST/PATCH/
	// DELETE /v1/services*). service-already-exists — UNIQUE-violation на
	// service_registry.name (REST TypeServiceExists, 409). not-found (нет записи
	// / CallerAID отсутствует в operators) / validation-failed (битый name/git/
	// ref/refresh) — общие коды.
	mcpCodeServiceExists = "service-already-exists"

	// mcpCodeOmenExists — UNIQUE-violation на omens.name (REST TypeOmenExists,
	// 409). not-found Omen / Rite — общий mcpCodeNotFound; validation — общий
	// mcpCodeValidationFailed. Augur CRUD (ADR-025, augur.md).
	mcpCodeOmenExists = "omen-already-exists"

	// mcpCodeVigilExists / mcpCodeDecreeExists — UNIQUE-violation на vigils.name /
	// decrees.name (REST TypeVigilExists / TypeDecreeExists, 409). not-found Vigil /
	// Decree — общий mcpCodeNotFound; validation — общий mcpCodeValidationFailed.
	// Oracle CRUD (ADR-030, beacons S3).
	mcpCodeVigilExists  = "vigil-already-exists"
	mcpCodeDecreeExists = "decree-already-exists"

	// mcpCodePushProviderExists — UNIQUE-violation на push_providers.name (REST
	// TypePushProviderExists, 409). ADR-032 amendment 2026-05-26, S7-2.
	mcpCodePushProviderExists = "push-provider-already-exists"

	// mcpCodeProviderExists / mcpCodeProfileExists — UNIQUE-violation на
	// providers.name / profiles.name (REST TypeProviderExists / TypeProfileExists,
	// 409). not-found Provider / Profile (вкл. FK Profile→missing Provider, который
	// MCP profile.create отдаёт как validation-failed, parity REST 422) — общий
	// mcpCodeNotFound; валидация — общий mcpCodeValidationFailed. Cloud CRUD (ADR-017).
	mcpCodeProviderExists = "provider-already-exists"
	mcpCodeProfileExists  = "profile-already-exists"

	// mcpCodeProviderHasProfiles — удаление Provider-а заблокировано зависимыми
	// Profile-ями (REST TypeProviderHasProfiles, 409, FK RESTRICT). ADR-017.
	mcpCodeProviderHasProfiles = "provider-has-profiles"

	// mcpCodeHeraldExists / mcpCodeTidingExists — UNIQUE-violation на heralds.name /
	// tidings.name (REST TypeHeraldExists / TypeTidingExists, 409). not-found
	// Herald / Tiding (вкл. FK Tiding→missing Herald) — общий mcpCodeNotFound;
	// валидация — общий mcpCodeValidationFailed. ADR-052, S4.
	mcpCodeHeraldExists = "herald-already-exists"
	mcpCodeTidingExists = "tiding-already-exists"

	// mcpCodeErrandNotCancellable — попытка отменить Errand в терминальном
	// статусе (REST TypeErrandNotCancellable, 409). ADR-033 slice E5.
	mcpCodeErrandNotCancellable = "errand-not-cancellable"

	// mcpCodeMigrationFailed — зарезервированный код из mcp-tools.md § Errors.
	// СОЗНАТЕЛЬНО не задействован в mapIncarnationErrorToMCP: апгрейд
	// incarnation в статусе migration_failed возвращает [incarnation.
	// ErrIncarnationLocked] (тот же sentinel, что error_locked — см.
	// upgradeTx switch), который и REST, и MCP маппят в incarnation-locked.
	// Фейл самой migration-Apply отдаёт обёрнутую внутреннюю ошибку →
	// internal-error (паритет REST default-ветки Upgrade). Отдельная
	// классификация migration_failed на уровне error-mapping появится вместе
	// с дедик-sentinel-ом (изменение public-контракта error-mapping —
	// post-MVP, симметрично REST). Держим константу как канон URN-suffix-а
	// для будущей привязки; её присутствие в docs/keeper/mcp-tools.md § Errors
	// зашито тест-инвариантом (см. reservedMCPCodes ниже).
	mcpCodeMigrationFailed = "migration-failed"
)

// reservedMCPCodes — каноничные URN-suffix-ы, объявленные в mcp-tools.md
// § Errors, но ещё не привязанные к sentinel-у в mapIncarnationErrorToMCP /
// mapServiceErrorToMCP (привязка — when-needed, симметрично REST).
//
// Используется тест-инвариантом [TestReservedMCPCodes_PresentInDocs]: каждый
// reserved-код обязан присутствовать в таблице § Errors mcp-tools.md. Это
// ловит code↔doc drift (код объявлен, но в доках забыт, или наоборот) и
// заодно делает константу реально потребляемой, а не функционально мёртвой.
var reservedMCPCodes = []string{
	mcpCodeMigrationFailed,
}

// mcpToolError — payload, который transport кладёт в JSON-RPC `error.data`
// при tool-execution failure. Совмещён с MCP-tool error из mcp-tools.md:
// `code` — стабильный URN-suffix, `instance` — путь tool-а (для аудита).
//
// JSON-RPC-сторонний `error.code` мы держим как rpcCodeInternalError для
// всех tool-execution-ошибок (MCP-spec не определяет JSON-RPC-коды для
// прикладных ошибок); смысловой код — в data.code.
type mcpToolError struct {
	Code     string `json:"code"`
	Instance string `json:"instance,omitempty"`
}

// mapServiceErrorToMCP преобразует ошибку [operator.Service] в пару
// (MCP-code, public-сообщение). public-сообщение безопасно для возврата
// клиенту (не содержит internal stack / SQL-detail-ов).
//
// Для unknown-ошибок возвращается `internal-error` + generic-detail —
// raw err.Error() сюда не пробрасывается (oracle-attacks через различение
// internal-сообщений), это caller-side ответственность.
func mapServiceErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, operator.ErrOperatorAlreadyExists):
		return mcpCodeOperatorExists, "operator with this AID already exists"
	case errors.Is(err, operator.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator not found"
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return mcpCodeOperatorRevoked, "operator is already revoked"
	case errors.Is(err, operator.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "target is the last active cluster-admin; revoking would lock out the cluster"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	// Validation-error из service.Create/Revoke/IssueToken — fmt.Errorf
	// с префиксом "operator: invalid AID …" / "operator: ... is empty".
	// Распознаём по сообщению, без введения отдельного sentinel (это
	// фактически уже public-message, формируется в одном месте — service.go).
	// Префикс `operator: ` — internal-pkg-имя, его в публичный detail не
	// возвращаем: trim перед отдачей клиенту.
	if msg := err.Error(); strings.HasPrefix(msg, "operator: invalid AID ") ||
		strings.HasPrefix(msg, "operator: CallerAID is empty") {
		return mcpCodeValidationFailed, strings.TrimPrefix(msg, "operator: ")
	}
	return mcpCodeInternalError, "internal error"
}

// mapIncarnationErrorToMCP преобразует sentinel-ошибки incarnation-слоя
// (CRUD-tx + prepare-фаза [incarnation.PrepareUpgrade]) в пару
// (MCP-code, public-сообщение). Симметрично REST-handler-у
// IncarnationHandler: тот же набор sentinel-ов, те же смысловые коды.
//
// Соответствие REST problem-type ↔ MCP-code (docs/keeper/mcp-tools.md § Errors):
//   - TypeNotFound          → not-found.
//   - TypeIncarnationExists  → incarnation-already-exists.
//   - TypeIncarnationLocked  → incarnation-locked (все state-конфликты ресурса:
//     not-unlockable / busy / locked / downgrade / schema-mismatch).
//   - TypeValidationFailed   → validation-failed (no-op upgrade / broken chain).
//
// Внутренние сбои резолва (service-not-registered / load-failed / no-manifest /
// chain-load-failed / evaluator-failed) → internal-error: для клиента это
// «незапланированная ошибка», диагностика — в логах/OTel (caller-side).
//
// Для unknown-ошибок → internal-error + generic-detail (raw err.Error() не
// пробрасывается — oracle-attack-защита, как в mapServiceErrorToMCP).
func mapIncarnationErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, incarnation.ErrIncarnationNotFound):
		return mcpCodeNotFound, "incarnation not found"
	case errors.Is(err, incarnation.ErrIncarnationAlreadyExists):
		return mcpCodeIncarnationExists, "incarnation with this name already exists"
	case errors.Is(err, incarnation.ErrIncarnationNotLocked):
		return mcpCodeIncarnationLocked, "incarnation is not in an unlockable status — nothing to unlock"
	case errors.Is(err, incarnation.ErrIncarnationNotErrorLocked):
		return mcpCodeIncarnationLocked, "incarnation is not error_locked — rerun-create requires error_locked"
	case errors.Is(err, incarnation.ErrRerunScenarioNotCreate):
		return mcpCodeIncarnationLocked, "last failed scenario is not `create` — rerun-create restarts `create` only"
	case errors.Is(err, incarnation.ErrIncarnationBusy):
		return mcpCodeIncarnationLocked, "incarnation is applying — operation rejected until run completes"
	case errors.Is(err, incarnation.ErrIncarnationLocked):
		return mcpCodeIncarnationLocked, "incarnation is locked — unlock required before this operation"
	case errors.Is(err, incarnation.ErrDowngradeUnsupported),
		errors.Is(err, incarnation.ErrDowngradeViaRef):
		return mcpCodeIncarnationLocked, "to_version downgrades state_schema_version — forward-only (ADR-019)"
	case errors.Is(err, incarnation.ErrSchemaVersionMismatch):
		return mcpCodeIncarnationLocked, "incarnation schema changed concurrently — retry upgrade"
	case errors.Is(err, incarnation.ErrUpgradeNoop):
		return mcpCodeValidationFailed, "to_version matches current incarnation version — nothing to upgrade"
	case errors.Is(err, incarnation.ErrIncarnationNotDestroyable):
		// Статус не допускает destroy (applying / destroying) — state-конфликт
		// ресурса, тот же problem-type, что error_locked (REST TypeIncarnationLocked).
		return mcpCodeIncarnationLocked, "incarnation status does not allow destroy (applying / destroying)"
	case errors.Is(err, incarnation.ErrDestroyScenarioMissing):
		// allow_destroy=false и в снапшоте нет scenario `destroy` — teardown
		// выполнить нечем (REST TypeValidationFailed, 422).
		return mcpCodeValidationFailed, "service snapshot has no `destroy` scenario — pass allow_destroy=true to force destroy without teardown"
	case errors.Is(err, artifact.ErrMigrationChainBroken):
		return mcpCodeValidationFailed, "migration chain to target version is broken"
	case errors.Is(err, incarnation.ErrServiceNotRegistered):
		return mcpCodeInternalError, "service is not registered"
	case errors.Is(err, incarnation.ErrLoadTargetSnapshot),
		errors.Is(err, incarnation.ErrTargetSnapshotInvalid),
		errors.Is(err, incarnation.ErrLoadMigrationChain),
		errors.Is(err, incarnation.ErrBuildEvaluator):
		return mcpCodeInternalError, "internal error"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	return mcpCodeInternalError, "internal error"
}

// mapRoleErrorToMCP преобразует sentinel-ошибки RBAC-CRUD-слоя ([rbac.Service])
// в пару (MCP-code, public-сообщение). Симметрично mapServiceErrorToMCP /
// mapIncarnationErrorToMCP: тот же набор sentinel-ов, что у REST-handler-а
// role-tools (Slice 2a), те же смысловые коды.
//
// Соответствие sentinel ↔ MCP-code:
//   - ErrRoleNotFound / ErrRoleOperatorNotFound / ErrOperatorNotFound → not-found.
//   - ErrRoleAlreadyExists                                             → role-already-exists.
//   - ErrRoleBuiltin                                                   → role-builtin.
//   - ErrWouldLockOutCluster                                          → would-lock-out-cluster
//     (переиспользуем общий код с operator-self-lockout-ом).
//   - ErrInvalidRoleName + wrapped ParsePermission-ошибка              → validation-failed.
//   - ErrPermissionNotHeld (least-privilege subset-check)              → forbidden.
//   - ErrPermissionDenied                                              → forbidden.
//
// Для unknown-ошибок → internal-error + generic-detail (raw err.Error() не
// пробрасывается — oracle-attack-защита, как в соседних мапперах).
func mapRoleErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, rbac.ErrRoleNotFound):
		return mcpCodeNotFound, "role not found"
	case errors.Is(err, rbac.ErrRoleOperatorNotFound):
		return mcpCodeNotFound, "role-operator membership not found"
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator (AID) not found"
	case errors.Is(err, rbac.ErrRoleAlreadyExists):
		return mcpCodeRoleExists, "role with this name already exists"
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return mcpCodeRoleBuiltin, "role is builtin — delete/update forbidden"
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "operation would leave the cluster without an active operator holding '*' permission"
	case errors.Is(err, rbac.ErrInvalidRoleName):
		return mcpCodeValidationFailed, "invalid role name"
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return mcpCodeForbidden, "cannot grant a permission you do not hold yourself"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	// Битый permission — wrapped ParsePermission-ошибка с префиксом
	// "rbac: invalid permission …" (формируется в одном месте — service.go/
	// crud.go). Это уже public-message; внутренний pkg-префикс trim-аем перед
	// отдачей клиенту. Отдельного sentinel-а нет (текст несёт диагностику).
	if msg := err.Error(); strings.HasPrefix(msg, "rbac: invalid permission ") {
		return mcpCodeValidationFailed, strings.TrimPrefix(msg, "rbac: ")
	}
	return mcpCodeInternalError, "internal error"
}

// mapSynodErrorToMCP преобразует sentinel-ошибки Synod-CRUD-слоя ([rbac.Service],
// ADR-049) в пару (MCP-code, public-сообщение). Симметрично mapRoleErrorToMCP:
// те же смысловые коды, что у REST-handler-а synod-tools.
//
// Соответствие sentinel ↔ MCP-code:
//   - ErrSynodNotFound / ErrSynodOperatorNotFound / ErrSynodRoleNotFound /
//     ErrOperatorNotFound / ErrRoleNotFound                        → not-found.
//   - ErrSynodAlreadyExists                                        → synod-already-exists.
//   - ErrSynodBuiltin                                              → synod-builtin.
//   - ErrWouldLockOutCluster                                       → would-lock-out-cluster.
//   - ErrInvalidSynodName                                          → validation-failed.
//   - ErrPermissionNotHeld (least-privilege subset)                → forbidden.
//   - ErrPermissionDenied                                          → forbidden.
//
// not-found для ErrSynodNotFound отделён от ErrSynodOperatorNotFound/
// ErrSynodRoleNotFound кодом-же not-found, но разным detail (диагностика). Для
// unknown → internal-error + generic-detail (oracle-attack-защита).
func mapSynodErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, rbac.ErrSynodNotFound):
		return mcpCodeSynodNotFound, "synod not found"
	case errors.Is(err, rbac.ErrSynodOperatorNotFound):
		return mcpCodeNotFound, "synod-operator membership not found"
	case errors.Is(err, rbac.ErrSynodRoleNotFound):
		return mcpCodeNotFound, "synod-role bundle entry not found"
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator (AID) not found"
	case errors.Is(err, rbac.ErrRoleNotFound):
		return mcpCodeNotFound, "role not found"
	case errors.Is(err, rbac.ErrSynodAlreadyExists):
		return mcpCodeSynodExists, "synod with this name already exists"
	case errors.Is(err, rbac.ErrSynodBuiltin):
		return mcpCodeSynodBuiltin, "synod is builtin — delete forbidden"
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "operation would leave the cluster without an active operator holding '*' permission"
	case errors.Is(err, rbac.ErrInvalidSynodName):
		return mcpCodeValidationFailed, "invalid synod name"
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return mcpCodeForbidden, "cannot grant permissions you do not hold yourself"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSoulErrorToMCP преобразует sentinel-ошибки soul-онбординга
// ([soul.*] + [bootstraptoken.*]) в пару (MCP-code, public-сообщение).
// Симметрично REST-handler-у SoulHandler (Create / IssueToken): тот же набор
// sentinel-ов, те же смысловые коды.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrSoulAlreadyExists    → soul-already-exists (REST TypeSoulExists).
//   - ErrSoulCreatorNotFound  → validation-failed (REST TypeValidationFailed:
//     AID создателя отсутствует в реестре operators).
//   - ErrSoulNotFound         → not-found (REST TypeNotFound).
//   - ErrTokenActiveExists    → bootstrap-token-active (REST
//     TypeBootstrapTokenActive: активный токен есть, force не указан).
//
// Для unknown-ошибок → internal-error + generic-detail (raw err.Error() не
// пробрасывается — oracle-attack-защита, как в соседних мапперах).
func mapSoulErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, soul.ErrSoulAlreadyExists):
		return mcpCodeSoulExists, "soul with this SID already exists"
	case errors.Is(err, soul.ErrSoulCreatorNotFound):
		return mcpCodeValidationFailed, "creator AID not found in operators registry"
	case errors.Is(err, soul.ErrSoulNotFound):
		return mcpCodeNotFound, "soul not found"
	case errors.Is(err, bootstraptoken.ErrTokenActiveExists):
		return mcpCodeBootstrapTokenActive, "soul already has an active bootstrap token; pass force=true to expire it and reissue"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSigilErrorToMCP преобразует sentinel-ошибки Sigil-allow-list-слоя
// ([sigil.Service]) в пару (MCP-code, public-сообщение). Симметрично
// REST-handler-у SigilHandler (Allow / Revoke): тот же набор sentinel-ов, те же
// смысловые коды.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrPluginNotInCache    → plugin-not-in-cache (REST TypePluginNotInCache).
//   - ErrSigilAlreadyActive  → sigil-already-active (REST TypeSigilActive).
//   - ErrSigilNotFound       → sigil-not-found (REST TypeSigilNotFound).
//
// Для unknown-ошибок → internal-error + generic-detail (raw err.Error() не
// пробрасывается — oracle-attack-защита, как в соседних мапперах).
func mapSigilErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, sigil.ErrPluginNotInCache):
		return mcpCodePluginNotInCache, "plugin not found in host cache"
	case errors.Is(err, sigil.ErrSigilAlreadyActive):
		return mcpCodeSigilActive, "an active sigil already exists for (namespace, name, ref)"
	case errors.Is(err, sigil.ErrSigilNotFound):
		return mcpCodeSigilNotFound, "no active sigil for (namespace, name, ref)"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSigilKeyErrorToMCP преобразует sentinel-ошибки ротации ключей подписи
// ([sigil.KeyService]) в пару (MCP-code, public-сообщение). Симметрично
// REST-handler-у SigilKeyHandler: тот же набор sentinel-ов, те же смысловые коды.
//
//   - ErrKeyNotFound       → sigil-key-not-found (REST TypeSigilKeyNotFound).
//   - ErrLastActiveKey     → sigil-key-last-active (REST TypeSigilKeyLastActive).
//   - ErrRetirePrimary     → sigil-key-primary (REST TypeSigilKeyPrimary).
//   - ErrConcurrentPrimary → sigil-key-concurrent-change (REST TypeSigilKeyConcurrentChange).
//   - ErrKeyRetired        → sigil-key-concurrent-change (retired-ключ на set-primary).
//
// Unknown → internal-error + generic-detail (raw err не пробрасывается).
func mapSigilKeyErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, sigil.ErrKeyNotFound):
		return mcpCodeSigilKeyNotFound, "no signing key with this key_id"
	case errors.Is(err, sigil.ErrLastActiveKey):
		return mcpCodeSigilKeyLastActive, "cannot retire the last active signing key"
	case errors.Is(err, sigil.ErrRetirePrimary):
		return mcpCodeSigilKeyPrimary, "cannot retire the primary key; set another key primary first"
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return mcpCodeSigilKeyConcurrentChange, "concurrent primary-key change; retry"
	case errors.Is(err, sigil.ErrKeyRetired):
		return mcpCodeSigilKeyConcurrentChange, "signing key is retired; cannot become primary"
	}
	return mcpCodeInternalError, "internal error"
}

// mapServiceRegistryErrorToMCP преобразует sentinel-ошибки реестра Service-ов
// ([serviceregistry.Service]) в пару (MCP-code, public-сообщение). Симметрично
// REST-handler-у ServiceHandler (Register / Update / Deregister): тот же набор
// sentinel-ов, те же смысловые коды.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrAlreadyExists      → service-already-exists (REST TypeServiceExists).
//   - ErrNotFound           → not-found (REST TypeNotFound: нет записи).
//   - ErrOperatorNotFound   → not-found (REST TypeNotFound: CallerAID
//     отсутствует в operators registry, FK-violation).
//   - ErrInvalidName / ErrInvalidGit / ErrInvalidRef / ErrInvalidRefresh →
//     validation-failed (REST TypeValidationFailed).
//
// Для unknown-ошибок → internal-error + generic-detail (raw err.Error() не
// пробрасывается — oracle-attack-защита, как в соседних мапперах).
func mapServiceRegistryErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, serviceregistry.ErrAlreadyExists):
		return mcpCodeServiceExists, "service with this name already exists"
	case errors.Is(err, serviceregistry.ErrNotFound):
		return mcpCodeNotFound, "service not found"
	case errors.Is(err, serviceregistry.ErrOperatorNotFound):
		return mcpCodeNotFound, "caller AID not found in operators registry"
	case errors.Is(err, serviceregistry.ErrInvalidName):
		return mcpCodeValidationFailed, "invalid service name"
	case errors.Is(err, serviceregistry.ErrInvalidGit):
		return mcpCodeValidationFailed, "git is empty"
	case errors.Is(err, serviceregistry.ErrInvalidRef):
		return mcpCodeValidationFailed, "ref is empty"
	case errors.Is(err, serviceregistry.ErrInvalidRefresh):
		return mcpCodeValidationFailed, "invalid refresh duration"
	}
	return mcpCodeInternalError, "internal error"
}

// mapAugurErrorToMCP преобразует sentinel-ошибки [augur.Service] (Omen / Rite
// CRUD) в пару (MCP-code, public-сообщение). Симметрично REST-handler-у
// AugurHandler.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrValidation        → validation-failed (REST TypeValidationFailed).
//   - ErrOmenAlreadyExists  → omen-already-exists (REST TypeOmenExists).
//   - ErrOmenNotFound       → not-found (REST TypeNotFound: Omen нет).
//   - ErrRiteNotFound       → not-found (REST TypeNotFound: Rite нет).
//
// ErrValidation несёт public-detail (errors.Unwrap уже public — формируется в
// augur.Service без internal SQL/stack), отдаём целиком. Для unknown-ошибок →
// internal-error + generic-detail (oracle-attack-защита, как в соседях).
func mapAugurErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, augur.ErrValidation):
		return mcpCodeValidationFailed, strings.TrimPrefix(err.Error(), "augur: ")
	case errors.Is(err, augur.ErrOmenAlreadyExists):
		return mcpCodeOmenExists, "omen with this name already exists"
	case errors.Is(err, augur.ErrOmenNotFound):
		return mcpCodeNotFound, "omen not found"
	case errors.Is(err, augur.ErrRiteNotFound):
		return mcpCodeNotFound, "rite not found"
	}
	return mcpCodeInternalError, "internal error"
}

// mapOracleErrorToMCP преобразует sentinel-ошибки [oracle.Service] (Vigil /
// Decree CRUD) в пару (MCP-code, public-сообщение). Симметрично REST-handler-у
// OracleHandler.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrValidation          → validation-failed (REST TypeValidationFailed).
//   - ErrVigilAlreadyExists    → vigil-already-exists (REST TypeVigilExists).
//   - ErrDecreeAlreadyExists   → decree-already-exists (REST TypeDecreeExists).
//   - ErrVigilNotFound        → not-found (REST TypeNotFound).
//   - ErrDecreeNotFound       → not-found (REST TypeNotFound).
//
// ErrValidation несёт public-detail (errors.Unwrap уже public — формируется в
// oracle.Service без internal SQL/stack), отдаём целиком. Для unknown-ошибок →
// internal-error + generic-detail (oracle-attack-защита, как в соседях).
func mapOracleErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, oracle.ErrValidation):
		return mcpCodeValidationFailed, strings.TrimPrefix(err.Error(), "oracle: ")
	case errors.Is(err, oracle.ErrVigilAlreadyExists):
		return mcpCodeVigilExists, "vigil with this name already exists"
	case errors.Is(err, oracle.ErrDecreeAlreadyExists):
		return mcpCodeDecreeExists, "decree with this name already exists"
	case errors.Is(err, oracle.ErrVigilNotFound):
		return mcpCodeNotFound, "vigil not found"
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return mcpCodeNotFound, "decree not found"
	}
	return mcpCodeInternalError, "internal error"
}

// mapHeraldErrorToMCP преобразует sentinel-ошибки [herald.Service] (Herald CRUD)
// в пару (MCP-code, public-сообщение). Симметрично REST-handler-у HeraldHandler.
//
// Соответствие sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrHeraldExists    → herald-already-exists (REST TypeHeraldExists).
//   - ErrHeraldNotFound  → not-found (REST TypeNotFound).
//   - ErrValidation      → validation-failed (REST TypeValidationFailed).
//
// ErrValidation несёт public-detail (формируется валидаторами без internal SQL/
// stack — herald.PublicMessage). Для unknown-ошибок → internal-error +
// generic-detail (oracle-attack-защита, как в соседях).
func mapHeraldErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, herald.ErrHeraldExists):
		return mcpCodeHeraldExists, "herald with this name already exists"
	case errors.Is(err, herald.ErrHeraldNotFound):
		return mcpCodeNotFound, "herald not found"
	case herald.IsValidationError(err):
		return mcpCodeValidationFailed, herald.PublicMessage(err)
	}
	return mcpCodeInternalError, "internal error"
}

// mapTidingErrorToMCP преобразует sentinel-ошибки [herald.Service] (Tiding CRUD)
// в пару (MCP-code, public-сообщение). Симметрично REST-handler-у.
//
//   - ErrTidingExists    → tiding-already-exists (REST TypeTidingExists).
//   - ErrTidingNotFound  → not-found (Tiding нет).
//   - ErrHeraldNotFound  → not-found (FK Tiding→missing Herald, REST TypeNotFound).
//   - ErrValidation      → validation-failed.
//
// ErrHeraldNotFound проверяется ДО ErrTidingNotFound (FK-violation на
// отсутствующий herald — отдельный смысл). Unknown → internal-error.
func mapTidingErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, herald.ErrTidingExists):
		return mcpCodeTidingExists, "tiding with this name already exists"
	case errors.Is(err, herald.ErrHeraldNotFound):
		return mcpCodeNotFound, "referenced herald not found"
	case errors.Is(err, herald.ErrTidingNotFound):
		return mcpCodeNotFound, "tiding not found"
	case herald.IsValidationError(err):
		return mcpCodeValidationFailed, herald.PublicMessage(err)
	}
	return mcpCodeInternalError, "internal error"
}
