package api

// Served-механизм GET /openapi.yaml и GET /openapi.json. Спека отдаётся из
// КОДА — runtime-дамп агрегатора huma-операций ([HumaFullSpecYAML] /
// [buildFullOpenAPISpec], FastAPI-стиль), OpenAPI 3.1.0. Источник один —
// huma-агрегатор; committed docs/keeper/openapi.yaml — его производный снимок
// для UI-vendor (make gen-openapi), а не отдельная рукопись.
//
// YAML vs JSON: оба варианта собираются из ОДНОГО source-of-truth (huma-
// агрегатор). YAML (.YAML()) — для людей и тулов; JSON (json.Marshal) — для
// inline-рендера вьювера /docs (RapiDoc loadSpec ждёт ОБЪЕКТ, JSON парсится на
// клиенте; YAML-текст RapiDoc трактовал бы как URL). huma .YAML() сам под
// капотом маршалит JSON → конвертит в YAML, так что оба формата эквивалентны.
//
// КЕШ: huma-reflection дорогая, а спека неизменна за жизнь процесса (операции
// регистрируются статически). Поэтому каждый дамп собирается РОВНО ОДИН РАЗ
// через sync.Once при первом обращении и кешируется в []byte; каждый
// последующий GET отдаёт буфер без пересборки.
//
// БЕЗОПАСНОСТЬ: оба роута за JWT (security в спеке) — монтируются ПРЯМЫМ
// chi-mount ВНЕ группы /v1 c RequireJWT, поэтому НЕ несут RBAC/audit (см.
// router.go).

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
)

// contentTypeOpenAPI — application/yaml per RFC 9512. Держим локально, чтобы
// served-хендлер был самодостаточен.
const contentTypeOpenAPI = "application/yaml; charset=utf-8"

// contentTypeOpenAPIJSON — application/json для JSON-варианта спеки (inline-
// рендер RapiDoc на /docs).
const contentTypeOpenAPIJSON = "application/json; charset=utf-8"

// servedSpec лениво собирает и кеширует YAML-дамп полной huma-спеки. buildErr
// фиксируется один раз: если первая сборка упала (коллизия merge — гейт а/б
// агрегатора), хендлер отдаёт 500, а не пересобирает на каждый запрос.
var servedSpec struct {
	once  sync.Once
	bytes []byte
	err   error
}

// servedSpecJSON — JSON-аналог servedSpec (тот же source-of-truth, отдельный
// кеш и Once, т.к. формат другой).
var servedSpecJSON struct {
	once  sync.Once
	bytes []byte
	err   error
}

// openAPISpecBytes возвращает закешированный YAML-дамп полной huma-спеки,
// собирая его при первом вызове. Потокобезопасно (sync.Once).
func openAPISpecBytes() ([]byte, error) {
	servedSpec.once.Do(func() {
		y, err := HumaFullSpecYAML()
		if err != nil {
			servedSpec.err = err
			return
		}
		servedSpec.bytes = []byte(y)
	})
	return servedSpec.bytes, servedSpec.err
}

// openAPISpecJSONBytes возвращает закешированный JSON-дамп полной huma-спеки.
// Source-of-truth — тот же buildFullOpenAPISpec, что и у YAML; *huma.OpenAPI
// реализует MarshalJSON, поэтому json.Marshal даёт каноничный 3.1-JSON.
func openAPISpecJSONBytes() ([]byte, error) {
	servedSpecJSON.once.Do(func() {
		spec, err := buildFullOpenAPISpec()
		if err != nil {
			servedSpecJSON.err = err
			return
		}
		b, err := json.Marshal(spec)
		if err != nil {
			servedSpecJSON.err = err
			return
		}
		servedSpecJSON.bytes = b
	})
	return servedSpecJSON.bytes, servedSpecJSON.err
}

// servedOpenAPIHandler отдаёт закешированный 3.1-дамп huma-спеки со статусом 200
// (Content-Type application/yaml, Content-Length из длины буфера). Метод GET —
// единственный; router.go вешает через r.Get(...). Если агрегатор не собрался
// (merge-коллизия) — 500 без тела спеки.
func servedOpenAPIHandler(w http.ResponseWriter, _ *http.Request) {
	spec, err := openAPISpecBytes()
	if err != nil {
		http.Error(w, "openapi spec assembly failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeOpenAPI)
	w.Header().Set("Content-Length", strconv.Itoa(len(spec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}

// servedOpenAPIJSONHandler — JSON-аналог servedOpenAPIHandler. Нужен вьюверу
// /docs: RapiDoc.loadSpec принимает ОБЪЕКТ, страница парсит этот JSON и подаёт
// разобранный объект (строку RapiDoc трактовал бы как spec-URL). 200/500 как
// у YAML-варианта.
func servedOpenAPIJSONHandler(w http.ResponseWriter, _ *http.Request) {
	spec, err := openAPISpecJSONBytes()
	if err != nil {
		http.Error(w, "openapi spec assembly failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeOpenAPIJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(spec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}
