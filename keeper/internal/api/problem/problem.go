// Package problem — RFC 7807 «Problem Details for HTTP APIs»
// (application/problem+json) для Operator API.
//
// Нормативная привязка типов ошибок — [docs/keeper/operator-api.md →
// Типы ошибок]. Список `Type*`-констант — only-add: новые типы можно
// добавлять, существующие не переименовывать (machine-clients парсят
// `type` URL как стабильный идентификатор).
//
// M0.6a покрывает только базовые 4xx/5xx-типы (auth/not-found/malformed/
// internal). Доменные типы (`incarnation-locked`, `would-lock-out-cluster`,
// `operator-already-exists`, etc) — M0.6b/c при появлении соответствующих
// endpoint-ов.
package problem

import (
	"encoding/json"
	"net/http"
)

// ContentType — RFC 7807 §3 media type для problem-ответов.
const ContentType = "application/problem+json"

// Каталог `type`-URN-ов. Стабильные, версионируются only-add.
// Домен `https://soul-stack.io/errors/<suffix>` зафиксирован в
// [operator-api.md → § Error format (RFC 7807)].
const (
	TypeUnauthenticated  = "https://soul-stack.io/errors/unauthenticated"
	TypeForbidden        = "https://soul-stack.io/errors/forbidden"
	TypeNotFound         = "https://soul-stack.io/errors/not-found"
	TypeMethodNotAllowed = "https://soul-stack.io/errors/method-not-allowed"
	TypeMalformedRequest = "https://soul-stack.io/errors/malformed-request"
	TypeValidationFailed = "https://soul-stack.io/errors/validation-failed"
	TypeInternalError    = "https://soul-stack.io/errors/internal-error"
	TypeOperatorExists   = "https://soul-stack.io/errors/operator-already-exists"
	TypeOperatorRevoked  = "https://soul-stack.io/errors/operator-revoked"
	// TypeOperatorRevokedToken — JWT валиден по подписи/exp, но AID Архонта
	// ревокнут в реестре `operators` (ADR-014 Amendment 2026-05-27, JWT
	// immediate revoke). 401 (parity с expired JWT), а НЕ 403 — токен больше
	// не доверенный. Отдельный URN от [TypeOperatorRevoked] (409 на write-
	// side IssueToken/Revoke для уже ревокнутого AID), чтобы UI/SDK мог
	// различить ситуацию «свой токен сгорел → пора в логин» от «нельзя
	// выполнить операцию над чужим ревокнутым AID».
	TypeOperatorRevokedToken = "https://soul-stack.io/errors/operator-revoked-token"
	TypeWouldLockOutCluster  = "https://soul-stack.io/errors/would-lock-out-cluster"
	TypeIncarnationExists    = "https://soul-stack.io/errors/incarnation-already-exists"
	TypeIncarnationLocked    = "https://soul-stack.io/errors/incarnation-locked"
	// TypeRerunInputUnavailable — `rerun-last` не может восстановить input упавшего
	// day-2-прогона: рецепт (`apply_runs.recipe`) недоступен (recipe IS NULL на
	// legacy-пути dispatchWave ЛИБО строка вычищена Reaper-ретеншном). 409 Conflict,
	// как и [TypeIncarnationLocked] (тот же HTTP-класс «целевое состояние
	// недостижимо»), но отдельный URN: оба кейса rerun-last были неразличимы
	// machine-readable (UI вынужденно матчил подстроку detail). Отделяет
	// «input упавшего прогона утрачен → unlock + ручной run» от «статус не
	// error_locked → unlock» (последний остаётся на [TypeIncarnationLocked]).
	TypeRerunInputUnavailable = "https://soul-stack.io/errors/rerun-input-unavailable"
	TypeSoulExists            = "https://soul-stack.io/errors/soul-already-exists"
	TypeBootstrapTokenActive  = "https://soul-stack.io/errors/bootstrap-token-active"
	TypeRoleNotFound          = "https://soul-stack.io/errors/role-not-found"
	TypeRoleExists            = "https://soul-stack.io/errors/role-already-exists"
	TypeRoleBuiltin           = "https://soul-stack.io/errors/role-builtin"
	TypeSynodNotFound         = "https://soul-stack.io/errors/synod-not-found"
	TypeSynodExists           = "https://soul-stack.io/errors/synod-already-exists"
	TypeSynodBuiltin          = "https://soul-stack.io/errors/synod-builtin"
	TypeSigilActive           = "https://soul-stack.io/errors/sigil-already-active"
	TypeSigilNotFound         = "https://soul-stack.io/errors/sigil-not-found"
	TypePluginNotInCache      = "https://soul-stack.io/errors/plugin-not-in-cache"
	TypeServiceExists         = "https://soul-stack.io/errors/service-already-exists"
	// Augur — реестр Omen / Rite (ADR-025, augur.md). omen-already-exists —
	// UNIQUE на omens.name (409). not-found Omen / Rite — общий TypeNotFound.
	TypeOmenExists = "https://soul-stack.io/errors/omen-already-exists"
	// Oracle — реестры Vigil / Decree (ADR-030, beacons S3). *-already-exists —
	// UNIQUE на vigils.name / decrees.name (409). not-found — общий TypeNotFound.
	TypeVigilExists  = "https://soul-stack.io/errors/vigil-already-exists"
	TypeDecreeExists = "https://soul-stack.io/errors/decree-already-exists"
	// Ротация ключей подписи Sigil (ADR-026(h), R3-S7).
	TypeSigilKeyNotFound         = "https://soul-stack.io/errors/sigil-key-not-found"
	TypeSigilKeyLastActive       = "https://soul-stack.io/errors/sigil-key-last-active"
	TypeSigilKeyPrimary          = "https://soul-stack.io/errors/sigil-key-primary"
	TypeSigilKeyConcurrentChange = "https://soul-stack.io/errors/sigil-key-concurrent-change"
	// `GET /v1/souls/{sid}/soulprint`: запись Soul-а есть, но typed-SoulprintReport
	// ни разу не приходил (410 Gone). Различение от 404: сам Soul зарегистрирован,
	// но фактов пока нет (только что онбординг / `transport: ssh` без агента).
	TypeSoulprintNotReceived = "https://soul-stack.io/errors/soulprint-not-received"
	// TypeClusterDegraded — кластер в degraded-режиме (Toll, ADR-038): rate
	// массового оттока Soul-ов превысил threshold за окно, write-API временно
	// блокирован (503 Service Unavailable + Retry-After). Read-API, RBAC,
	// destroy, Errand остаются доступны (recovery actions).
	TypeClusterDegraded = "https://soul-stack.io/errors/cluster-degraded"
	// TypePushProviderExists — UNIQUE-violation на push_providers.name (409,
	// ADR-032 amendment 2026-05-26, S7-2). Симметрия с TypeServiceExists /
	// TypeOperatorExists.
	TypePushProviderExists = "https://soul-stack.io/errors/push-provider-already-exists"
	// Cloud Provider / Profile — operator-facing CRUD реестров providers /
	// profiles (ADR-017, docs/keeper/cloud.md).
	// *-already-exists — UNIQUE-violation на providers.name / profiles.name
	// (409, симметрия с TypePushProviderExists / TypeServiceExists). not-found
	// Provider / Profile — общий TypeNotFound; FK Profile→missing Provider —
	// тоже TypeNotFound (ErrProviderNotFound из profile, parity Tiding→Herald);
	// битый name/type/credentials_ref — общий TypeValidationFailed (422).
	TypeProviderExists = "https://soul-stack.io/errors/provider-already-exists"
	TypeProfileExists  = "https://soul-stack.io/errors/profile-already-exists"
	// TypeProviderHasProfiles — удаление Provider-а заблокировано зависимыми
	// Profile-ями (FK profiles_provider_fk ON DELETE RESTRICT, миграция 020).
	// 409 Conflict: «целевое состояние недостижимо, есть зависимости» —
	// оператор обязан сначала удалить профили.
	TypeProviderHasProfiles = "https://soul-stack.io/errors/provider-has-profiles"
	// TypeErrandNotCancellable — попытка отменить Errand, уже находящийся в
	// терминальном статусе (DELETE /v1/errands/{errand_id}, ADR-033 slice E5).
	// 409 Conflict — корректный код «целевое состояние недостижимо».
	TypeErrandNotCancellable = "https://soul-stack.io/errors/errand-not-cancellable"
	// TypeBadGateway — keeper исправен, но внешний git-источник вернул ошибку
	// (`GET /v1/services/{name}/refs` → ls-remote). 502 Bad Gateway — корректный
	// код «upstream сервис недоступен»; в detail прокидывается оригинальная
	// причина (DNS / auth / неподдерживаемая схема — все «not our fault»).
	TypeBadGateway = "https://soul-stack.io/errors/bad-gateway"
	// Choir/Voice — топология хостов внутри инкарнации (ADR-044, S-T3).
	// *-already-exists — UNIQUE по PK `incarnation_choirs` / `incarnation_choir_voices`
	// (409). not-found Choir / Voice / incarnation — общий TypeNotFound. SID-ы вне
	// членства инкарнации (ErrNotMembers) — общий TypeValidationFailed (422).
	TypeChoirExists = "https://soul-stack.io/errors/choir-already-exists"
	TypeVoiceExists = "https://soul-stack.io/errors/voice-already-exists"
	// TypeTempoExceeded — per-AID rate-limit Tempo превышен (ADR-050): оператор
	// слишком часто дёргает resolver-тяжёлый write-эндпоинт. 429 Too Many
	// Requests + заголовок Retry-After (секунды до пополнения хотя бы одного
	// токена). Отдельный URN и код от [TypeClusterDegraded] (503 cluster-wide
	// по здоровью кластера): Tempo — 429 per-AID по частоте; единый problem+json/
	// Retry-After-каркас, разный риск.
	TypeTempoExceeded = "https://soul-stack.io/errors/tempo-exceeded"
	// TypeAuthThrottled — anti-bruteforce-лимит публичного login-эндпоинта
	// превышен (ADR-058(g), HIGH-3): слишком частые попытки с IP/username ИЛИ
	// lockout после серии неудач. 429 Too Many Requests + Retry-After (секунды
	// до снятия). Отдельный URN от [TypeTempoExceeded] (per-AID, post-JWT):
	// auth-throttle — pre-JWT по IP/username; anti-oracle — detail без причины
	// (не раскрываем, по IP это или по username, locked или throttled).
	TypeAuthThrottled = "https://soul-stack.io/errors/auth-throttled"
	// Herald/Tiding — уведомления о событиях прогонов (ADR-052, S4).
	// *-already-exists — UNIQUE-violation на heralds.name / tidings.name (409,
	// симметрия с TypeOmenExists / TypePushProviderExists). not-found Herald /
	// Tiding — общий TypeNotFound; FK Tiding→missing Herald — тоже TypeNotFound
	// (ErrHeraldNotFound, parity Rite→missing Omen). Битый config / event_types /
	// secret_ref — общий TypeValidationFailed (422).
	TypeHeraldExists = "https://soul-stack.io/errors/herald-already-exists"
	TypeTidingExists = "https://soul-stack.io/errors/tiding-already-exists"
	// TypeProvisioningMethodDisabled — политика provisioning_allowed_methods
	// (ADR-058 Часть B) запретила СОЗДАНИЕ оператора этим методом. 403 Forbidden:
	// гейт на ветке создания (POST /v1/operators → user; federated auto-provision →
	// ldap/oidc). Отдельный URN от generic [TypeForbidden] (нет RBAC-права) — это
	// policy-отказ по способу провижининга, а не нехватка permission; UI/SDK может
	// различить «метод выключен политикой» от «недостаточно прав».
	TypeProvisioningMethodDisabled = "https://soul-stack.io/errors/provisioning-method-disabled"
	// TypeAssertFailed — scenario `assert:`-предикат не прошёл на pre-flight-
	// гейте СОЗДАНИЯ прогона (ADR-009/ADR-027 amendment 2026-06-23, форма A):
	// roster прогона не сходится с инвариантом топологии (напр. cluster size-
	// guard). 422 Unprocessable Entity — запрос синтаксически валиден, но
	// предусловие МОДЕЛИ не выполнено (parity `validation-failed`/input_invalid:
	// тот же 422-класс «семантика входа не сходится»). Отдельный URN от
	// `validation-failed`: UI/SDK различает «топология не сходится» (роняй на
	// roster/scope) от «поле input не матчит схему». incarnation НЕ создаётся,
	// fail-статус (error_locked) НЕ ставится — отказ на этапе модели ДО коммита.
	// НЕ 412 Precondition Failed (зарезервирован под conditional-headers
	// If-Match/If-None-Match — путать нельзя).
	TypeAssertFailed = "https://soul-stack.io/errors/assert-failed"
)

// titles — фиксированные английские заголовки для каждого known-`type`.
// Совпадают с нормативной спецификацией (operator-api.md).
var titles = map[string]string{
	TypeUnauthenticated:            "Authentication required",
	TypeForbidden:                  "Forbidden",
	TypeNotFound:                   "Resource not found",
	TypeMethodNotAllowed:           "Method not allowed",
	TypeMalformedRequest:           "Malformed request",
	TypeValidationFailed:           "Validation failed",
	TypeInternalError:              "Internal server error",
	TypeOperatorExists:             "Operator already exists",
	TypeOperatorRevoked:            "Operator is revoked",
	TypeOperatorRevokedToken:       "Operator revoked",
	TypeWouldLockOutCluster:        "Operation would lock out the cluster",
	TypeIncarnationExists:          "Incarnation already exists",
	TypeIncarnationLocked:          "Incarnation is locked",
	TypeRerunInputUnavailable:      "Rerun input unavailable",
	TypeSoulExists:                 "Soul already exists",
	TypeBootstrapTokenActive:       "Bootstrap token already active",
	TypeRoleNotFound:               "Role not found",
	TypeRoleExists:                 "Role already exists",
	TypeRoleBuiltin:                "Role is builtin",
	TypeSynodNotFound:              "Synod not found",
	TypeSynodExists:                "Synod already exists",
	TypeSynodBuiltin:               "Synod is builtin",
	TypeSigilActive:                "Sigil already active",
	TypeSigilNotFound:              "Sigil not found",
	TypePluginNotInCache:           "Plugin not found in host cache",
	TypeServiceExists:              "Service already exists",
	TypeOmenExists:                 "Omen already exists",
	TypeVigilExists:                "Vigil already exists",
	TypeDecreeExists:               "Decree already exists",
	TypeSigilKeyNotFound:           "Sigil signing key not found",
	TypeSigilKeyLastActive:         "Cannot retire the last active sigil signing key",
	TypeSigilKeyPrimary:            "Cannot retire the primary sigil signing key",
	TypeSigilKeyConcurrentChange:   "Concurrent primary-key change; retry",
	TypeSoulprintNotReceived:       "Soulprint not yet received",
	TypeClusterDegraded:            "Cluster is in degraded mode",
	TypePushProviderExists:         "Push provider already exists",
	TypeProviderExists:             "Cloud provider already exists",
	TypeProfileExists:              "Cloud profile already exists",
	TypeProviderHasProfiles:        "Cloud provider has dependent profiles",
	TypeErrandNotCancellable:       "Errand is not cancellable",
	TypeBadGateway:                 "Bad gateway",
	TypeChoirExists:                "Choir already exists",
	TypeVoiceExists:                "Voice already exists",
	TypeTempoExceeded:              "Too many requests",
	TypeAuthThrottled:              "Too many login attempts",
	TypeHeraldExists:               "Herald already exists",
	TypeTidingExists:               "Tiding already exists",
	TypeProvisioningMethodDisabled: "Provisioning method disabled by policy",
	TypeAssertFailed:               "Assertion failed",
}

// Details — JSON-форма RFC 7807-объекта. Поля строго по RFC; кастомные
// расширения (например, `errors[]` для validation-полей) добавятся как
// явные дополнительные поля в follow-up slice-ах при появлении нужды.
type Details struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// Write пишет p в ResponseWriter с Content-Type=`application/problem+json`.
// Не вызывает log.* и не оборачивает p — это ответственность caller-а
// (логирование делает error-middleware, не сам problem-пакет).
func Write(w http.ResponseWriter, p Details) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)
	// json.NewEncoder не вернёт ошибку для well-typed struct; если
	// transport-write упадёт (client разорвал соединение) — это уже
	// после WriteHeader, ошибку логировать некуда. Игнорируем сознательно.
	_ = json.NewEncoder(w).Encode(p)
}

// New собирает Details из type-URN и detail-сообщения. Title и status
// подставляются по [titles] / [statuses]. На unknown `t` возвращает
// Details с пустым Title и status=500 — caller-у нужно явно использовать
// одну из Type*-констант.
func New(t, instance, detail string) Details {
	return Details{
		Type:     t,
		Title:    titles[t],
		Status:   statuses[t],
		Detail:   detail,
		Instance: instance,
	}
}

// statuses — обратное отображение `type → HTTP status`. Совпадает с
// таблицей в operator-api.md.
var statuses = map[string]int{
	TypeUnauthenticated:            http.StatusUnauthorized,
	TypeForbidden:                  http.StatusForbidden,
	TypeNotFound:                   http.StatusNotFound,
	TypeMethodNotAllowed:           http.StatusMethodNotAllowed,
	TypeMalformedRequest:           http.StatusBadRequest,
	TypeValidationFailed:           http.StatusUnprocessableEntity,
	TypeInternalError:              http.StatusInternalServerError,
	TypeOperatorExists:             http.StatusConflict,
	TypeOperatorRevoked:            http.StatusConflict,
	TypeOperatorRevokedToken:       http.StatusUnauthorized,
	TypeWouldLockOutCluster:        http.StatusConflict,
	TypeIncarnationExists:          http.StatusConflict,
	TypeIncarnationLocked:          http.StatusConflict,
	TypeRerunInputUnavailable:      http.StatusConflict,
	TypeSoulExists:                 http.StatusConflict,
	TypeBootstrapTokenActive:       http.StatusConflict,
	TypeRoleNotFound:               http.StatusNotFound,
	TypeRoleExists:                 http.StatusConflict,
	TypeRoleBuiltin:                http.StatusConflict,
	TypeSynodNotFound:              http.StatusNotFound,
	TypeSynodExists:                http.StatusConflict,
	TypeSynodBuiltin:               http.StatusConflict,
	TypeSigilActive:                http.StatusConflict,
	TypeSigilNotFound:              http.StatusNotFound,
	TypePluginNotInCache:           http.StatusNotFound,
	TypeServiceExists:              http.StatusConflict,
	TypeOmenExists:                 http.StatusConflict,
	TypeVigilExists:                http.StatusConflict,
	TypeDecreeExists:               http.StatusConflict,
	TypeSigilKeyNotFound:           http.StatusNotFound,
	TypeSigilKeyLastActive:         http.StatusConflict,
	TypeSigilKeyPrimary:            http.StatusConflict,
	TypeSigilKeyConcurrentChange:   http.StatusConflict,
	TypeSoulprintNotReceived:       http.StatusGone,
	TypeClusterDegraded:            http.StatusServiceUnavailable,
	TypePushProviderExists:         http.StatusConflict,
	TypeProviderExists:             http.StatusConflict,
	TypeProfileExists:              http.StatusConflict,
	TypeProviderHasProfiles:        http.StatusConflict,
	TypeErrandNotCancellable:       http.StatusConflict,
	TypeBadGateway:                 http.StatusBadGateway,
	TypeChoirExists:                http.StatusConflict,
	TypeVoiceExists:                http.StatusConflict,
	TypeTempoExceeded:              http.StatusTooManyRequests,
	TypeAuthThrottled:              http.StatusTooManyRequests,
	TypeHeraldExists:               http.StatusConflict,
	TypeTidingExists:               http.StatusConflict,
	TypeProvisioningMethodDisabled: http.StatusForbidden,
	TypeAssertFailed:               http.StatusUnprocessableEntity,
}
